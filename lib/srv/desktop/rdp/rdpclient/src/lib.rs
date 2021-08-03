#[macro_use]
extern crate lazy_static;

use libc::{fd_set, select, FD_SET};
use rdp::core::client::{Connector, RdpClient};
use rdp::core::event::*;
use rdp::model::error::*;
use std::collections::HashMap;
use std::mem;
use std::net::TcpStream;
use std::os::unix::io::AsRawFd;
use std::ptr;
use std::sync::{Arc, Mutex};

struct Client {
    rdp_client: RdpClient<TcpStream>,
    tcp_fd: usize,
}
type SyncRdpClient = Arc<Mutex<Client>>;

lazy_static! {
    static ref RDP_CLIENTS: Arc<Mutex<HashMap<i64, SyncRdpClient>>> =
        Arc::new(Mutex::new(HashMap::new()));
}

fn register_client(client_ref: i64, client: Client) {
    RDP_CLIENTS
        .lock()
        .unwrap()
        .insert(client_ref, Arc::new(Mutex::new(client)));
}

fn unregister_client(client_ref: &i64) {
    RDP_CLIENTS.lock().unwrap().remove(client_ref);
}

fn with_client<F: FnMut(&SyncRdpClient)>(client_ref: &i64, mut f: F) {
    match RDP_CLIENTS.lock().unwrap().get(client_ref) {
        Some(client) => f(client),
        None => {
            println!("attempt to use unregistered client {}", client_ref);
        }
    }
}

fn wait_for_fd(fd: usize) -> bool {
    unsafe {
        let mut raw_fds: fd_set = mem::zeroed();

        FD_SET(fd as i32, &mut raw_fds);

        let result = select(
            fd as i32 + 1,
            &mut raw_fds,
            ptr::null_mut(),
            ptr::null_mut(),
            ptr::null_mut(),
        );
        result == 1
    }
}

#[repr(C)]
pub struct CGOString {
    data: *mut u8,
    len: u16,
}

impl From<CGOString> for String {
    fn from(s: CGOString) -> String {
        unsafe { String::from_raw_parts(s.data, s.len.into(), s.len.into()) }
    }
}

#[repr(C)]
pub struct Bitmap {
    pub dest_left: u16,
    pub dest_top: u16,
    pub dest_right: u16,
    pub dest_bottom: u16,
    pub data_ptr: *const u8,
    pub data_len: usize,
}

#[no_mangle]
pub extern "C" fn connect_rdp(
    go_addr: CGOString,
    go_username: CGOString,
    go_password: CGOString,
    screen_width: u16,
    screen_height: u16,
    client_ref: i64,
) {
    // Convert from C to Rust types.
    let addr = String::from(go_addr);
    let username = String::from(go_username);
    let password = String::from(go_password);

    // Connect and authenticate.
    let tcp = TcpStream::connect(addr).unwrap();
    let tcp_fd = tcp.as_raw_fd() as usize;
    let mut connector = Connector::new()
        .screen(screen_width, screen_height)
        .credentials(".".to_string(), username.to_string(), password.to_string());
    let client = connector.connect(tcp).unwrap();

    register_client(
        client_ref,
        Client {
            rdp_client: client,
            tcp_fd: tcp_fd,
        },
    );
}

#[no_mangle]
pub extern "C" fn read_rdp_output(
    client_ref: i64,
    handle_bitmap: unsafe extern "C" fn(i64, Bitmap),
) {
    let mut tcp_fd = 0;
    with_client(&client_ref, |client| {
        tcp_fd = client.lock().unwrap().tcp_fd;
    });
    // Read incoming events.
    while wait_for_fd(tcp_fd as usize) {
        with_client(&client_ref, |client| {
            if let Err(Error::RdpError(e)) =
                client
                    .lock()
                    .unwrap()
                    .rdp_client
                    .read(|rdp_event| match rdp_event {
                        RdpEvent::Bitmap(bitmap) => {
                            // TODO: implement Into trait
                            let mut cbitmap = Bitmap {
                                dest_left: bitmap.dest_left,
                                dest_top: bitmap.dest_top,
                                dest_right: bitmap.dest_right,
                                dest_bottom: bitmap.dest_bottom,
                                data_ptr: std::ptr::null(),
                                data_len: 0,
                            };

                            let data = if bitmap.is_compress {
                                bitmap.decompress().unwrap()
                            } else {
                                bitmap.data
                            };
                            cbitmap.data_ptr = data.as_ptr();
                            cbitmap.data_len = data.len();
                            unsafe { handle_bitmap(client_ref, cbitmap) };
                        }
                        RdpEvent::Pointer(pointer) => {
                            println!("got pointer x: {} y: {}", pointer.x, pointer.y);
                        }
                        RdpEvent::Key(key) => {
                            println!("got key code {}", key.code);
                        }
                    })
            {
                match e.kind() {
                    RdpErrorKind::Disconnect => {}
                    _ => println!("{:?}", e),
                }
            }
        })
    }
}

#[repr(C)]
#[derive(Copy, Clone)]
pub struct Pointer {
    pub x: u16,
    pub y: u16,
    pub button: CGOPointerButton,
    pub down: bool,
}

#[repr(C)]
#[derive(Copy, Clone)]
pub enum CGOPointerButton {
    PointerButtonNone,
    PointerButtonLeft,
    PointerButtonRight,
    PointerButtonMiddle,
}

impl From<Pointer> for PointerEvent {
    fn from(p: Pointer) -> PointerEvent {
        PointerEvent {
            x: p.x,
            y: p.y,
            button: match p.button {
                CGOPointerButton::PointerButtonNone => PointerButton::None,
                CGOPointerButton::PointerButtonLeft => PointerButton::Left,
                CGOPointerButton::PointerButtonRight => PointerButton::Right,
                CGOPointerButton::PointerButtonMiddle => PointerButton::Middle,
            },
            down: p.down,
        }
    }
}

#[no_mangle]
pub extern "C" fn write_rdp_pointer(client_ref: i64, pointer: Pointer) {
    with_client(&client_ref, |client| {
        client
            .lock()
            .unwrap()
            .rdp_client
            .write(RdpEvent::Pointer(pointer.into()))
            .unwrap();
    });
}

#[repr(C)]
#[derive(Copy, Clone)]
pub struct Key {
    pub code: u16,
    pub down: bool,
}

impl From<Key> for KeyboardEvent {
    fn from(k: Key) -> KeyboardEvent {
        KeyboardEvent {
            code: k.code,
            down: k.down,
        }
    }
}

#[no_mangle]
pub extern "C" fn write_rdp_keyboard(client_ref: i64, key: Key) {
    with_client(&client_ref, |client| {
        client
            .lock()
            .unwrap()
            .rdp_client
            .write(RdpEvent::Key(key.into()))
            .unwrap();
    });
}

#[no_mangle]
pub extern "C" fn close_rdp(client_ref: i64) {
    with_client(&client_ref, |client| {
        client.lock().unwrap().rdp_client.shutdown().unwrap();
    });
    unregister_client(&client_ref);
}
