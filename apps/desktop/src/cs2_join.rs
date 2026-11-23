use std::net::SocketAddrV4;

pub fn steam_url(address: &str) -> Result<String, &'static str> {
    let addr: SocketAddrV4 = address
        .parse()
        .map_err(|_| "Invalid Varde server address.")?;
    if !addr.ip().is_loopback() || addr.port() == 0 {
        return Err("Invalid Varde server address.");
    }
    // Launch app 730 with a direct connection, without querying the server browser.
    Ok(format!("steam://run/730//+connect%20{addr}"))
}

#[cfg(test)]
mod tests {
    use super::steam_url;

    #[test]
    fn opens_cs2_with_the_varde_address() {
        assert_eq!(
            steam_url("127.77.123.42:27015").unwrap(),
            "steam://run/730//+connect%20127.77.123.42:27015"
        );
    }

    #[test]
    fn rejects_invalid_addresses_and_extra_arguments() {
        for address in [
            "127.77.123.42:27015%20+quit",
            "127.77.123.42:27015 +quit",
            "127.77.123.42:0",
            "127.77.123.42",
            "192.0.2.10:27015",
        ] {
            assert!(steam_url(address).is_err(), "accepted {address}");
        }
    }
}
