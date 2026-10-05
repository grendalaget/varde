//! p2pgames-testgame: tiny dedicated "game server" used by tests and the e2e
//! demo. Mirrors the Minecraft save-barrier protocol shape:
//!
//!   TCP :7777 — line protocol: `INCR`, `GET`, `SET <n>` → replies the value
//!   UDP :7777 — `GET` datagram → value
//!   stdin   — `save-off`, `save-all` (prints "Saved the game"), `save-on`,
//!             `stop`
//!   state   — `data/state.json`, autosaved on change when saving is on

use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, UdpSocket};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};

static VALUE: AtomicI64 = AtomicI64::new(0);
static AUTOSAVE: AtomicBool = AtomicBool::new(true);

fn state_file() -> PathBuf {
    PathBuf::from("data").join("state.json")
}

fn load() {
    if let Ok(s) = std::fs::read_to_string(state_file()) {
        if let Ok(v) = serde_json::from_str::<serde_json::Value>(&s) {
            if let Some(n) = v.get("value").and_then(|v| v.as_i64()) {
                VALUE.store(n, Ordering::SeqCst);
            }
        }
    }
}

fn save() {
    let dir = PathBuf::from("data");
    let _ = std::fs::create_dir_all(&dir);
    let body = serde_json::json!({ "value": VALUE.load(Ordering::SeqCst) }).to_string();
    let tmp = dir.join("state.json.tmp");
    if std::fs::write(&tmp, &body).is_ok() {
        let _ = std::fs::rename(&tmp, state_file());
    }
}

fn touch() {
    if AUTOSAVE.load(Ordering::SeqCst) {
        save();
    }
}

fn handle_line(line: &str) -> String {
    let mut it = line.split_whitespace();
    match it.next().unwrap_or("") {
        "INCR" => {
            let v = VALUE.fetch_add(1, Ordering::SeqCst) + 1;
            touch();
            v.to_string()
        }
        "GET" => VALUE.load(Ordering::SeqCst).to_string(),
        "SET" => {
            let n: i64 = it.next().and_then(|s| s.parse().ok()).unwrap_or(0);
            VALUE.store(n, Ordering::SeqCst);
            touch();
            n.to_string()
        }
        _ => "ERR".into(),
    }
}

fn main() {
    let port: u16 = std::env::args()
        .skip_while(|a| a != "--port")
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(7777);
    load();
    let tcp = TcpListener::bind(("127.0.0.1", port)).expect("bind tcp");
    let udp = UdpSocket::bind(("127.0.0.1", port)).expect("bind udp");
    udp.set_nonblocking(true).ok();
    println!("testgame listening on :{port}");

    // stdin commands
    std::thread::spawn(move || {
        for line in BufReader::new(std::io::stdin()).lines() {
            let Ok(line) = line else { break };
            match line.trim() {
                "save-off" => AUTOSAVE.store(false, Ordering::SeqCst),
                "save-all" => {
                    save();
                    println!("Saved the game");
                }
                "save-on" => AUTOSAVE.store(true, Ordering::SeqCst),
                "stop" => {
                    save();
                    println!("Stopping");
                    std::process::exit(0);
                }
                _ => {}
            }
        }
        // stdin closed → parent wants us gone
        std::process::exit(0);
    });

    let udp2 = udp.try_clone().unwrap();
    std::thread::spawn(move || {
        let mut buf = [0u8; 1500];
        loop {
            match udp2.recv_from(&mut buf) {
                Ok((n, src)) => {
                    let req = String::from_utf8_lossy(&buf[..n]);
                    let resp = handle_line(&req);
                    let _ = udp2.send_to(resp.as_bytes(), src);
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(std::time::Duration::from_millis(1));
                }
                Err(_) => break,
            }
        }
    });

    for c in tcp.incoming() {
        let Ok(c) = c else { continue };
        std::thread::spawn(move || {
            let mut r = BufReader::new(c.try_clone().unwrap());
            let mut w = c;
            let mut line = String::new();
            loop {
                line.clear();
                match r.read_line(&mut line) {
                    Ok(0) | Err(_) => break,
                    Ok(_) => {
                        let resp = handle_line(&line);
                        if w.write_all(resp.as_bytes()).is_err() || w.write_all(b"\n").is_err() {
                            break;
                        }
                        let _ = w.flush();
                    }
                }
            }
        });
    }
}
