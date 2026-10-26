use game_driver_api::Result;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

async fn send(stream: &mut TcpStream, id: i32, kind: i32, text: &str) -> Result<()> {
    let len = i32::try_from(text.len() + 10)?;
    stream.write_all(&len.to_le_bytes()).await?;
    stream.write_all(&id.to_le_bytes()).await?;
    stream.write_all(&kind.to_le_bytes()).await?;
    stream.write_all(text.as_bytes()).await?;
    stream.write_all(&[0, 0]).await?;
    Ok(())
}

async fn receive(stream: &mut TcpStream) -> Result<(i32, i32)> {
    let len = stream.read_i32_le().await?;
    if !(10..=4096).contains(&len) {
        return Err("cs2: invalid RCON packet length".into());
    }
    let mut packet = vec![0; len as usize];
    stream.read_exact(&mut packet).await?;
    if packet[packet.len() - 2..] != [0, 0] {
        return Err("cs2: invalid RCON packet terminator".into());
    }
    Ok((
        i32::from_le_bytes(packet[..4].try_into()?),
        i32::from_le_bytes(packet[4..8].try_into()?),
    ))
}

pub async fn exec(port: u16, password: &str, command: &str) -> Result<()> {
    let mut stream = TcpStream::connect((std::net::Ipv4Addr::LOCALHOST, port)).await?;
    send(&mut stream, 1, 3, password).await?;
    let mut authenticated = false;
    for _ in 0..2 {
        match receive(&mut stream).await? {
            (1, 2) => {
                authenticated = true;
                break;
            }
            (1, 0) => (),
            _ => return Err("cs2: RCON authentication failed".into()),
        }
    }
    if !authenticated {
        return Err("cs2: missing RCON authentication response".into());
    }
    send(&mut stream, 2, 2, command).await?;
    if receive(&mut stream).await? != (2, 0) {
        return Err("cs2: invalid RCON command response".into());
    }
    send(&mut stream, 3, 2, "rcon_password \"\"").await?;
    for _ in 0..8 {
        match receive(&mut stream).await {
            Ok((2, 0)) => (),
            Ok((3, 0)) => return Ok(()),
            Err(e)
                if e.downcast_ref::<std::io::Error>()
                    .is_some_and(|e| e.kind() == std::io::ErrorKind::UnexpectedEof) =>
            {
                return Ok(());
            }
            Err(e) => return Err(e),
            _ => return Err("cs2: invalid RCON shutdown response".into()),
        }
    }
    Err("cs2: missing RCON shutdown response".into())
}
