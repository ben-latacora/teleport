/*
Copyright 2020-2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package db

import (
	"context"
	"crypto/tls"
	"net"
	"sync"

	"github.com/gravitational/teleport"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/labels"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/srv/db/common"
	"github.com/gravitational/teleport/lib/srv/db/mongodb"
	"github.com/gravitational/teleport/lib/srv/db/mysql"
	"github.com/gravitational/teleport/lib/srv/db/postgres"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/pborman/uuid"
	"github.com/sirupsen/logrus"
)

// Config is the configuration for an database proxy server.
type Config struct {
	// Clock used to control time.
	Clock clockwork.Clock
	// DataDir is the path to the data directory for the server.
	DataDir string
	// AuthClient is a client directly connected to the Auth server.
	AuthClient *auth.Client
	// AccessPoint is a caching client connected to the Auth Server.
	AccessPoint auth.AccessPoint
	// StreamEmitter is a non-blocking audit events emitter.
	StreamEmitter events.StreamEmitter
	// NewAudit allows to override audit logger in tests.
	NewAudit NewAuditFn
	// TLSConfig is the *tls.Config for this server.
	TLSConfig *tls.Config
	// Authorizer is used to authorize requests coming from proxy.
	Authorizer auth.Authorizer
	// GetRotation returns the certificate rotation state.
	GetRotation func(role types.SystemRole) (*types.Rotation, error)
	// Server is the resource representing this database server.
	Server types.DatabaseServer
	// OnHeartbeat is called after every heartbeat. Used to update process state.
	OnHeartbeat func(error)
	// Auth is responsible for generating database auth tokens.
	Auth common.Auth
	// CADownloader automatically downloads root certs for cloud hosted databases.
	CADownloader CADownloader
	// LockWatcher is a lock watcher.
	LockWatcher *services.LockWatcher
}

// NewAuditFn defines a function that creates an audit logger.
type NewAuditFn func(common.AuditConfig) (common.Audit, error)

// CheckAndSetDefaults makes sure the configuration has the minimum required
// to function.
func (c *Config) CheckAndSetDefaults(ctx context.Context) (err error) {
	if c.Clock == nil {
		c.Clock = clockwork.NewRealClock()
	}
	if c.DataDir == "" {
		return trace.BadParameter("missing DataDir")
	}
	if c.AuthClient == nil {
		return trace.BadParameter("missing AuthClient")
	}
	if c.AccessPoint == nil {
		return trace.BadParameter("missing AccessPoint")
	}
	if c.StreamEmitter == nil {
		return trace.BadParameter("missing StreamEmitter")
	}
	if c.NewAudit == nil {
		c.NewAudit = common.NewAudit
	}
	if c.Auth == nil {
		c.Auth, err = common.NewAuth(common.AuthConfig{
			AuthClient: c.AuthClient,
			Clock:      c.Clock,
		})
		if err != nil {
			return trace.Wrap(err)
		}
	}
	if c.TLSConfig == nil {
		return trace.BadParameter("missing TLSConfig")
	}
	if c.Authorizer == nil {
		return trace.BadParameter("missing Authorizer")
	}
	if c.GetRotation == nil {
		return trace.BadParameter("missing GetRotation")
	}
	if c.Server == nil {
		return trace.BadParameter("missing Server")
	}
	if c.CADownloader == nil {
		c.CADownloader = NewRealDownloader(c.DataDir)
	}
	if c.LockWatcher == nil {
		return trace.BadParameter("missing LockWatcher")
	}
	return nil
}

// Server is a database server. It accepts database client requests coming over
// reverse tunnel from Teleport proxy and proxies them to databases.
type Server struct {
	// cfg is the database server configuration.
	cfg Config
	// closeContext is used to indicate the server is closing.
	closeContext context.Context
	// closeFunc is the cancel function of the close context.
	closeFunc context.CancelFunc
	// middleware extracts identity from client certificates.
	middleware *auth.Middleware
	// dynamicLabels contains dynamic labels for databases.
	dynamicLabels map[string]*labels.Dynamic
	// heartbeats holds hearbeats for database servers.
	heartbeats map[string]*srv.Heartbeat
	// mu protects access to server infos.
	mu sync.RWMutex
	// log is used for logging.
	log *logrus.Entry
}

// New returns a new database server.
func New(ctx context.Context, config Config) (*Server, error) {
	err := config.CheckAndSetDefaults(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	ctx, cancel := context.WithCancel(ctx)
	server := &Server{
		cfg:           config,
		log:           logrus.WithField(trace.Component, teleport.ComponentDatabase),
		closeContext:  ctx,
		closeFunc:     cancel,
		dynamicLabels: make(map[string]*labels.Dynamic),
		heartbeats:    make(map[string]*srv.Heartbeat),
		middleware: &auth.Middleware{
			AccessPoint:   config.AccessPoint,
			AcceptedUsage: []string{teleport.UsageDatabaseOnly},
		},
	}

	// Update TLS config to require client certificate.
	server.cfg.TLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
	server.cfg.TLSConfig.GetConfigForClient = getConfigForClient(
		server.cfg.TLSConfig, server.cfg.AccessPoint, server.log)

	// Initialize the heartbeat which will periodically register
	// this server with the auth server.
	if err := server.initHeartbeat(ctx, server.cfg.Server); err != nil {
		return nil, trace.Wrap(err)
	}

	// Perform various initialization actions on each proxied database, like
	// starting up dynamic labels and loading root certs for RDS dbs.
	for _, db := range server.cfg.Server.GetDatabases() {
		if err := server.initDatabase(ctx, db); err != nil {
			return nil, trace.Wrap(err, "failed to initialize %v", server)
		}
	}

	return server, nil
}

func (s *Server) initDatabase(ctx context.Context, database types.Database) error {
	if err := s.initDynamicLabels(ctx, database); err != nil {
		return trace.Wrap(err)
	}
	if err := s.initCACert(ctx, database); err != nil {
		return trace.Wrap(err)
	}
	s.log.Debugf("Initialized %v.", database)
	return nil
}

func (s *Server) initDynamicLabels(ctx context.Context, database types.Database) error {
	if len(database.GetDynamicLabels()) == 0 {
		return nil // Nothing to do.
	}
	dynamic, err := labels.NewDynamic(ctx, &labels.DynamicConfig{
		Labels: database.GetDynamicLabels(),
		Log:    s.log,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	dynamic.Sync()
	s.dynamicLabels[database.GetName()] = dynamic
	return nil
}

func (s *Server) initHeartbeat(ctx context.Context, server types.DatabaseServer) error {
	heartbeat, err := srv.NewHeartbeat(srv.HeartbeatConfig{
		Context:         s.closeContext,
		Component:       teleport.ComponentDatabase,
		Mode:            srv.HeartbeatModeDB,
		Announcer:       s.cfg.AccessPoint,
		GetServerInfo:   s.getServerInfoFunc(server),
		KeepAlivePeriod: apidefaults.ServerKeepAliveTTL,
		AnnouncePeriod:  apidefaults.ServerAnnounceTTL/2 + utils.RandomDuration(apidefaults.ServerAnnounceTTL/10),
		CheckPeriod:     defaults.HeartbeatCheckPeriod,
		ServerTTL:       apidefaults.ServerAnnounceTTL,
		OnHeartbeat:     s.cfg.OnHeartbeat,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	s.heartbeats[server.GetName()] = heartbeat
	return nil
}

func (s *Server) getServerInfoFunc(server types.DatabaseServer) func() (types.Resource, error) {
	return func() (types.Resource, error) {
		// Make sure to return a new object, because it gets cached by
		// heartbeat and will always compare as equal otherwise.
		s.mu.RLock()
		server = server.Copy()
		s.mu.RUnlock()
		// Update dynamic labels.
		databases := make([]types.Database, 0, len(server.GetDatabases()))
		for _, database := range server.GetDatabases() {
			labels, ok := s.dynamicLabels[database.GetName()]
			if ok {
				database.SetDynamicLabels(labels.Get())
			}
			databases = append(databases, database)
		}
		server.SetDatabases(databases)
		// Update CA rotation state.
		rotation, err := s.cfg.GetRotation(types.RoleDatabase)
		if err != nil && !trace.IsNotFound(err) {
			s.log.WithError(err).Warn("Failed to get rotation state.")
		} else {
			if rotation != nil {
				server.SetRotation(*rotation)
			}
		}
		// Update TTL.
		server.SetExpiry(s.cfg.Clock.Now().UTC().Add(apidefaults.ServerAnnounceTTL))
		return server, nil
	}
}

// Start starts heartbeating the presence of service.Databases that this
// server is proxying along with any dynamic labels.
func (s *Server) Start() error {
	for _, dynamicLabel := range s.dynamicLabels {
		go dynamicLabel.Start()
	}
	for _, heartbeat := range s.heartbeats {
		go heartbeat.Run()
	}
	return nil
}

// Close will shut the server down and unblock any resources.
func (s *Server) Close() error {
	// Stop dynamic label updates.
	for _, dynamicLabel := range s.dynamicLabels {
		dynamicLabel.Close()
	}
	// Signal to all goroutines to stop.
	s.closeFunc()
	// Stop the heartbeats.
	var errors []error
	for _, heartbeat := range s.heartbeats {
		errors = append(errors, heartbeat.Close())
	}
	errors = append(errors, s.cfg.Auth.Close())
	return trace.NewAggregate(errors...)
}

// Wait will block while the server is running.
func (s *Server) Wait() error {
	<-s.closeContext.Done()
	return s.closeContext.Err()
}

// HandleConnection accepts the connection coming over reverse tunnel,
// upgrades it to TLS, extracts identity information from it, performs
// authorization and dispatches to the appropriate database engine.
func (s *Server) HandleConnection(conn net.Conn) {
	log := s.log.WithField("addr", conn.RemoteAddr())
	log.Debug("Accepted connection.")
	// Upgrade the connection to TLS since the other side of the reverse
	// tunnel connection (proxy) will initiate a handshake.
	tlsConn := tls.Server(conn, s.cfg.TLSConfig)
	// Make sure to close the upgraded connection, not "conn", otherwise
	// the other side may not detect that connection has closed.
	defer tlsConn.Close()
	// Perform the hanshake explicitly, normally it should be performed
	// on the first read/write but when the connection is passed over
	// reverse tunnel it doesn't happen for some reason.
	err := tlsConn.Handshake()
	if err != nil {
		log.WithError(err).Error("Failed to perform TLS handshake.")
		return
	}
	// Now that the handshake has completed and the client has sent us a
	// certificate, extract identity information from it.
	ctx, err := s.middleware.WrapContextWithUser(s.closeContext, tlsConn)
	if err != nil {
		log.WithError(err).Error("Failed to extract identity from connection.")
		return
	}
	// Dispatch the connection for processing by an appropriate database
	// service.
	err = s.handleConnection(ctx, tlsConn)
	if err != nil && !utils.IsOKNetworkError(err) && !trace.IsAccessDenied(err) {
		log.WithError(err).Error("Failed to handle connection.")
		return
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) error {
	sessionCtx, err := s.authorize(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	streamWriter, err := s.newStreamWriter(sessionCtx)
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		// Closing the stream writer is needed to flush all recorded data
		// and trigger upload. Do it in a goroutine since depending on
		// session size it can take a while and we don't want to block
		// the client.
		go func() {
			// Use the server closing context to make sure that upload
			// continues beyond the session lifetime.
			err := streamWriter.Close(s.closeContext)
			if err != nil {
				sessionCtx.Log.WithError(err).Warn("Failed to close stream writer.")
			}
		}()
	}()
	engine, err := s.dispatch(sessionCtx, streamWriter)
	if err != nil {
		return trace.Wrap(err)
	}

	// Wrap a client connection into monitor that auto-terminates
	// idle connection and connection with expired cert.
	conn, err = monitorConn(ctx, monitorConnConfig{
		conn:         conn,
		lockWatcher:  s.cfg.LockWatcher,
		identity:     sessionCtx.Identity,
		checker:      sessionCtx.Checker,
		clock:        s.cfg.Clock,
		serverID:     s.cfg.Server.GetHostID(),
		authClient:   s.cfg.AuthClient,
		teleportUser: sessionCtx.Identity.Username,
		emitter:      s.cfg.AuthClient,
		log:          s.log,
		ctx:          s.closeContext,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	err = engine.HandleConnection(ctx, sessionCtx, conn)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// dispatch returns an appropriate database engine for the session.
func (s *Server) dispatch(sessionCtx *common.Session, streamWriter events.StreamWriter) (common.Engine, error) {
	audit, err := s.cfg.NewAudit(common.AuditConfig{
		Emitter: streamWriter,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	switch sessionCtx.Database.GetProtocol() {
	case defaults.ProtocolPostgres:
		return &postgres.Engine{
			Auth:    s.cfg.Auth,
			Audit:   audit,
			Context: s.closeContext,
			Clock:   s.cfg.Clock,
			Log:     sessionCtx.Log,
		}, nil
	case defaults.ProtocolMySQL:
		return &mysql.Engine{
			Auth:       s.cfg.Auth,
			Audit:      audit,
			AuthClient: s.cfg.AuthClient,
			Context:    s.closeContext,
			Clock:      s.cfg.Clock,
			Log:        sessionCtx.Log,
		}, nil
	case defaults.ProtocolMongoDB:
		return &mongodb.Engine{
			Auth:    s.cfg.Auth,
			Audit:   audit,
			Context: s.closeContext,
			Clock:   s.cfg.Clock,
			Log:     sessionCtx.Log,
		}, nil
	}
	return nil, trace.BadParameter("unsupported database protocol %q",
		sessionCtx.Database.GetProtocol())
}

func (s *Server) authorize(ctx context.Context) (*common.Session, error) {
	// Only allow local and remote identities to proxy to a database.
	userType := ctx.Value(auth.ContextUser)
	switch userType.(type) {
	case auth.LocalUser, auth.RemoteUser:
	default:
		return nil, trace.BadParameter("invalid identity: %T", userType)
	}
	// Extract authorizing context and identity of the user from the request.
	authContext, err := s.cfg.Authorizer.Authorize(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	identity := authContext.Identity.GetIdentity()
	s.log.Debugf("Client identity: %#v.", identity)
	// Fetch the requested database server.
	var database types.Database
	for _, db := range s.cfg.Server.GetDatabases() {
		if db.GetName() == identity.RouteToDatabase.ServiceName {
			database = db
		}
	}
	if database == nil {
		return nil, trace.NotFound("%q not found among registered databases: %v",
			identity.RouteToDatabase.ServiceName, s.cfg.Server.GetDatabases())
	}
	s.log.Debugf("Will connect to database %q at %v.", database.GetName(),
		database.GetURI())
	id := uuid.New()
	return &common.Session{
		ID:                id,
		ClusterName:       identity.RouteToCluster,
		Server:            s.cfg.Server,
		Database:          database,
		Identity:          identity,
		DatabaseUser:      identity.RouteToDatabase.Username,
		DatabaseName:      identity.RouteToDatabase.Database,
		Checker:           authContext.Checker,
		StartupParameters: make(map[string]string),
		Statements:        common.NewStatementsCache(),
		Log: s.log.WithFields(logrus.Fields{
			"id": id,
			"db": database.GetName(),
		}),
	}, nil
}
