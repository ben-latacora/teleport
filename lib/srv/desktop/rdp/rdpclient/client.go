package rdpclient

/*
#cgo LDFLAGS: -L${SRCDIR}/target/debug -l:librdp_client.a -lpthread -lcrypto -ldl -lssl -lm
#include <librdprs.h>

typedef void (*handleBitmap_callback)(int64_t, struct Bitmap);
void handleBitmap_cgo(int64_t cp, struct Bitmap cb);
*/
import "C"
import (
	"image"
	"log"
	"sync"
	"unsafe"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport/lib/srv/desktop/deskproto"
)

type Options struct {
	Addr          string
	Username      string
	Password      string
	ClientWidth   uint16
	ClientHeight  uint16
	OutputMessage func(deskproto.Message) error
	InputMessage  func() (deskproto.Message, error)
}

func (o Options) validate() error {
	if o.Addr == "" {
		return trace.BadParameter("missing Addr in rdpclient.Options")
	}
	if o.ClientWidth == 0 {
		return trace.BadParameter("missing ClientWidth in rdpclient.Options")
	}
	if o.ClientHeight == 0 {
		return trace.BadParameter("missing ClientHeight in rdpclient.Options")
	}
	if o.OutputMessage == nil {
		return trace.BadParameter("missing OutputMessage in rdpclient.Options")
	}
	return nil
}

type Client struct {
	opts      Options
	clientRef int64
	done      chan struct{}

	toFree []unsafe.Pointer
}

func New(opts Options) (*Client, error) {
	c := &Client{
		opts: opts,
		done: make(chan struct{}),
	}
	if err := c.connect(); err != nil {
		return nil, trace.Wrap(err)
	}
	go c.run()
	return c, nil
}

func (c *Client) connect() error {
	addr := cgoString(c.opts.Addr)
	c.toFree = append(c.toFree, unsafe.Pointer(addr.data))
	username := cgoString(c.opts.Username)
	c.toFree = append(c.toFree, unsafe.Pointer(username.data))
	password := cgoString(c.opts.Password)
	c.toFree = append(c.toFree, unsafe.Pointer(password.data))

	c.clientRef = registerClient(c)

	// TODO: return connection error
	C.connect_rdp(
		addr,
		username,
		password,
		C.uint16_t(c.opts.ClientWidth),
		C.uint16_t(c.opts.ClientHeight),
		C.int64_t(c.clientRef),
	)
	return nil
}

func (c *Client) run() {
	defer close(c.done)
	go C.read_rdp_output(
		C.int64_t(c.clientRef),
		(*[0]byte)(unsafe.Pointer(C.handleBitmap_cgo)),
	)

	var mouseX, mouseY uint32
	for {
		msg, err := c.opts.InputMessage()
		if err != nil {
			logrus.Warningf("Failed reading RDP input message: %v", err)
			return
		}
		switch m := msg.(type) {
		case deskproto.MouseMove:
			mouseX, mouseY = m.X, m.Y
			C.write_rdp_pointer(
				C.int64_t(c.clientRef),
				C.Pointer{
					x:      C.uint16_t(m.X),
					y:      C.uint16_t(m.Y),
					button: C.PointerButtonNone,
				},
			)
		case deskproto.MouseButton:
			var button C.CGOPointerButton
			switch m.Button {
			case deskproto.LeftMouseButton:
				button = C.PointerButtonLeft
			case deskproto.RightMouseButton:
				button = C.PointerButtonRight
			case deskproto.MiddleMouseButton:
				button = C.PointerButtonMiddle
			default:
				button = C.PointerButtonNone
			}
			C.write_rdp_pointer(
				C.int64_t(c.clientRef),
				C.Pointer{
					x:      C.uint16_t(mouseX),
					y:      C.uint16_t(mouseY),
					button: uint32(button),
					down:   m.State == deskproto.ButtonPressed,
				},
			)
		case deskproto.KeyboardButton:
			C.write_rdp_keyboard(
				C.int64_t(c.clientRef),
				C.Key{
					code: C.uint16_t(m.KeyCode),
					down: m.State == deskproto.ButtonPressed,
				},
			)
		}
	}
}

//export handleBitmapJump
func handleBitmapJump(ci C.int64_t, cb C.Bitmap) {
	findClient(int64(ci)).handleBitmap(cb)
}

func (c *Client) handleBitmap(cb C.Bitmap) {
	data := C.GoBytes(unsafe.Pointer(cb.data_ptr), C.int(cb.data_len))
	// Convert BGRA to RGBA.
	for i := 0; i < len(data); i += 4 {
		data[i], data[i+2] = data[i+2], data[i]
	}
	img := image.NewRGBA(image.Rectangle{
		Min: image.Pt(int(cb.dest_left), int(cb.dest_top)),
		Max: image.Pt(int(cb.dest_right)+1, int(cb.dest_bottom)+1),
	})
	copy(img.Pix, data)

	if err := c.opts.OutputMessage(deskproto.PNGFrame{Img: img}); err != nil {
		log.Printf("failed to send PNG frame %v: %v", img.Rect, err)
	}
}

func (c *Client) Wait() error {
	<-c.done

	C.close_rdp(C.int64_t(c.clientRef))
	unregisterClient(c.clientRef)
	for _, ptr := range c.toFree {
		C.free(ptr)
	}
	return nil
}

func cgoString(s string) C.CGOString {
	sb := []byte(s)
	return C.CGOString{
		data: (*C.uint8_t)(C.CBytes(sb)),
		len:  C.uint16_t(len(sb)),
	}
}

var (
	clientsMu    = &sync.RWMutex{}
	clients      = make(map[int64]*Client)
	clientsIndex = int64(-1)
)

func registerClient(c *Client) int64 {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	clientsIndex++
	clients[clientsIndex] = c
	return clientsIndex
}

func unregisterClient(i int64) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	delete(clients, i)
}

func findClient(i int64) *Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	return clients[i]
}
