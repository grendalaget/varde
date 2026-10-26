#![cfg(target_os = "linux")]

use std::ffi::CStr;
use std::sync::OnceLock;

fn port(value: &[u8]) -> Option<u16> {
    if value.is_empty() {
        return None;
    }
    let mut port = 0u16;
    for digit in value {
        if !digit.is_ascii_digit() {
            return None;
        }
        port = port.checked_mul(10)?.checked_add((digit - b'0').into())?;
    }
    (port != 0).then_some(port)
}

fn ipv4(address: u32, actual: u16, protected: u16) -> u32 {
    if actual == protected && address == 0 {
        u32::from_ne_bytes([127, 0, 0, 1])
    } else {
        address
    }
}

fn ipv6(address: [u8; 16], actual: u16, protected: u16, v6only: bool) -> [u8; 16] {
    let mapped_any = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 0, 0, 0, 0];
    if actual != protected || (address != [0; 16] && address != mapped_any) {
        return address;
    }
    if v6only && address == [0; 16] {
        let mut local = [0; 16];
        local[15] = 1;
        local
    } else {
        [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 127, 0, 0, 1]
    }
}

#[no_mangle]
pub extern "C" fn varde_cs2_bind_version() -> u32 {
    1
}

/// # Safety
/// The address must satisfy the libc bind contract.
#[no_mangle]
pub unsafe extern "C" fn bind(
    fd: libc::c_int,
    address: *const libc::sockaddr,
    length: libc::socklen_t,
) -> libc::c_int {
    type Bind =
        unsafe extern "C" fn(libc::c_int, *const libc::sockaddr, libc::socklen_t) -> libc::c_int;
    static NEXT: OnceLock<Option<Bind>> = OnceLock::new();
    let Some(next) = NEXT.get_or_init(|| {
        let symbol = libc::dlsym(libc::RTLD_NEXT, c"bind".as_ptr());
        (!symbol.is_null()).then(|| std::mem::transmute::<*mut libc::c_void, Bind>(symbol))
    }) else {
        *libc::__errno_location() = libc::ENOSYS;
        return -1;
    };
    let value = libc::getenv(c"VARDE_CS2_BIND_PORT".as_ptr());
    let protected = if value.is_null() {
        None
    } else {
        port(CStr::from_ptr(value).to_bytes())
    };
    let Some(protected) = protected else {
        if !value.is_null() {
            *libc::__errno_location() = libc::EINVAL;
            return -1;
        }
        return next(fd, address, length);
    };
    if address.is_null() || (length as usize) < std::mem::size_of::<libc::sa_family_t>() {
        return next(fd, address, length);
    }
    let family = std::ptr::read_unaligned(address.cast::<libc::sa_family_t>()) as i32;
    if family == libc::AF_INET && length as usize >= std::mem::size_of::<libc::sockaddr_in>() {
        let mut local = std::ptr::read_unaligned(address.cast::<libc::sockaddr_in>());
        local.sin_addr.s_addr = ipv4(
            local.sin_addr.s_addr,
            u16::from_be(local.sin_port),
            protected,
        );
        return next(
            fd,
            (&local as *const libc::sockaddr_in).cast(),
            std::mem::size_of_val(&local) as libc::socklen_t,
        );
    }
    if family == libc::AF_INET6 && length as usize >= std::mem::size_of::<libc::sockaddr_in6>() {
        let mut local = std::ptr::read_unaligned(address.cast::<libc::sockaddr_in6>());
        let mut v6only: libc::c_int = 1;
        let mut size = std::mem::size_of_val(&v6only) as libc::socklen_t;
        if libc::getsockopt(
            fd,
            libc::IPPROTO_IPV6,
            libc::IPV6_V6ONLY,
            (&mut v6only as *mut libc::c_int).cast(),
            &mut size,
        ) != 0
        {
            return -1;
        }
        local.sin6_addr.s6_addr = ipv6(
            local.sin6_addr.s6_addr,
            u16::from_be(local.sin6_port),
            protected,
            v6only != 0,
        );
        return next(
            fd,
            (&local as *const libc::sockaddr_in6).cast(),
            std::mem::size_of_val(&local) as libc::socklen_t,
        );
    }
    next(fd, address, length)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_protected_wildcards_are_rewritten() {
        let local = u32::from_ne_bytes([127, 0, 0, 1]);
        assert_eq!(ipv4(0, 27015, 27015), local);
        assert_eq!(ipv4(local, 27015, 27015), local);
        for actual in [0, 27005, 27020, 49152] {
            assert_eq!(ipv4(0, actual, 27015), 0);
            assert_eq!(ipv6([0; 16], actual, 27015, false), [0; 16]);
        }
        let mapped_local = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 127, 0, 0, 1];
        let mapped_any = [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 0, 0, 0, 0];
        let mut local6 = [0; 16];
        local6[15] = 1;
        assert_eq!(ipv6([0; 16], 27015, 27015, true), local6);
        assert_eq!(ipv6([0; 16], 27015, 27015, false), mapped_local);
        assert_eq!(ipv6(mapped_any, 27015, 27015, false), mapped_local);
        assert_eq!(ipv6(local6, 27015, 27015, false), local6);
        let public = [1; 16];
        assert_eq!(ipv6(public, 27015, 27015, false), public);
    }

    #[test]
    fn ports_must_be_nonzero_decimal_u16() {
        assert_eq!(port(b"27015"), Some(27015));
        for value in [b"".as_slice(), b"0", b"65536", b"-1", b"27015,27016", b"x"] {
            assert_eq!(port(value), None);
        }
    }
}
