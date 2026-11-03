use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::{Error, Result};

#[derive(Debug, Deserialize)]
pub struct ProxyRequest {
    pub method: String,
    pub path: String,
    pub body: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ProxyResponse {
    pub status: u16,
    pub body: String,
}

#[derive(Debug, PartialEq, Eq)]
pub enum SessionChange {
    Keep,
    Set(String),
    Clear,
}

fn hex_value(byte: u8) -> Option<u8> {
    match byte {
        b'0'..=b'9' => Some(byte - b'0'),
        b'a'..=b'f' => Some(byte - b'a' + 10),
        b'A'..=b'F' => Some(byte - b'A' + 10),
        _ => None,
    }
}

fn decode_path_segment(segment: &str) -> Result<String> {
    let bytes = segment.as_bytes();
    let mut decoded = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] != b'%' {
            decoded.push(bytes[index]);
            index += 1;
            continue;
        }
        let Some((&high, &low)) = bytes.get(index + 1).zip(bytes.get(index + 2)) else {
            return Err(Error::Url("path contains an invalid percent escape".into()));
        };
        let (Some(high), Some(low)) = (hex_value(high), hex_value(low)) else {
            return Err(Error::Url("path contains an invalid percent escape".into()));
        };
        decoded.push(high * 16 + low);
        index += 3;
    }
    String::from_utf8(decoded)
        .map_err(|_| Error::Url("path contains an invalid UTF-8 escape".into()))
}

pub fn validate_path(path: &str) -> Result<()> {
    let pathname = path.split_once('?').map_or(path, |(pathname, _)| pathname);
    if !pathname.starts_with("/v1/") {
        return Err(Error::Url("path must start with /v1/".into()));
    }
    if path.chars().any(char::is_control) || path.contains('#') {
        return Err(Error::Url("path contains a forbidden character".into()));
    }
    if pathname.contains("//") || pathname.contains('\\') {
        return Err(Error::Url("path contains a forbidden separator".into()));
    }
    for segment in pathname.split('/') {
        let decoded = decode_path_segment(segment)?;
        if decoded == "."
            || decoded == ".."
            || decoded
                .chars()
                .any(|character| matches!(character, '/' | '\\') || character.is_control())
        {
            return Err(Error::Url("path contains a forbidden path segment".into()));
        }
    }
    Ok(())
}

pub fn session_change(
    method: &str,
    path: &str,
    status: u16,
    body: &[u8],
) -> (SessionChange, Vec<u8>) {
    let pathname = path.split_once('?').map_or(path, |(pathname, _)| pathname);
    if method == "POST" && pathname == "/v1/auth/logout" {
        return (SessionChange::Clear, body.to_vec());
    }
    if method == "GET" && pathname == "/v1/me" && status == 401 {
        return (SessionChange::Clear, body.to_vec());
    }
    if method != "POST"
        || !(200..300).contains(&status)
        || !matches!(
            pathname,
            "/v1/auth/login" | "/v1/auth/signup" | "/v1/auth/external"
        )
    {
        return (SessionChange::Keep, body.to_vec());
    }

    let Ok(mut value) = serde_json::from_slice::<Value>(body) else {
        return (SessionChange::Keep, body.to_vec());
    };
    let Some(object) = value.as_object_mut() else {
        return (SessionChange::Keep, body.to_vec());
    };
    let Some(token) = object
        .get("token")
        .and_then(Value::as_str)
        .map(str::to_owned)
    else {
        return (SessionChange::Keep, body.to_vec());
    };
    object.remove("token");
    let sanitized = serde_json::to_vec(&value).expect("JSON values always serialize");
    (SessionChange::Set(token), sanitized)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_path_accepts_api_paths_and_queries() {
        for path in [
            "/v1/me",
            "/v1/groups/group_1/events/stream",
            "/v1/groups/a%20b",
            "/v1/search?q=home%2Faway&next=../page",
            "/v1/search?q=group//name",
        ] {
            assert!(validate_path(path).is_ok(), "{path}");
        }
    }

    #[test]
    fn validate_path_rejects_non_api_paths() {
        for path in ["", "/", "/v1", "/v2/me", "v1/me"] {
            assert!(validate_path(path).is_err(), "{path:?}");
        }
    }

    #[test]
    fn validate_path_rejects_unsafe_path_parts() {
        for path in [
            "/v1//me",
            "/v1/me\\admin",
            "/v1/a/../me",
            "/v1/a/..",
            "/v1/me#fragment",
            "/v1/me\r\nx: y",
            "/v1/me\0",
            "/v1/me\u{7f}",
            "/v1/%2e%2e/admin",
            "/v1/.%2e/admin",
            "/v1/%2E./x",
            "/v1/%2e/x",
            "/v1/%2e%2e/me",
            "/v1/%2E%2e/me",
            "/v1/groups%2fadmin",
            "/v1/a%2fb",
            "/v1/a%5cb",
            "/v1/%zz",
            "/v1/%2",
        ] {
            assert!(validate_path(path).is_err(), "{path:?}");
        }
    }

    #[test]
    fn login_sets_session_and_strips_token() {
        let (change, body) = session_change(
            "POST",
            "/v1/auth/login",
            200,
            br#"{"token":"secret","user":{"id":"u1"}}"#,
        );
        assert_eq!(change, SessionChange::Set("secret".into()));
        assert_eq!(
            serde_json::from_slice::<Value>(&body).unwrap(),
            serde_json::json!({"user":{"id":"u1"}})
        );
    }

    #[test]
    fn signup_sets_session() {
        let (change, body) =
            session_change("POST", "/v1/auth/signup", 201, br#"{"token":"signup"}"#);
        assert_eq!(change, SessionChange::Set("signup".into()));
        assert_eq!(body, b"{}");
    }

    #[test]
    fn external_login_sets_session() {
        let (change, body) =
            session_change("POST", "/v1/auth/external", 200, br#"{"token":"external"}"#);
        assert_eq!(change, SessionChange::Set("external".into()));
        assert_eq!(body, b"{}");
    }

    #[test]
    fn failed_login_keeps_session_and_body() {
        let body = br#"{"error":"unauthorized"}"#;
        let (change, returned) = session_change("POST", "/v1/auth/login", 401, body);
        assert_eq!(change, SessionChange::Keep);
        assert_eq!(returned, body);
    }

    #[test]
    fn logout_clears_session_for_any_status() {
        let body = b"logged out";
        let (change, returned) = session_change("POST", "/v1/auth/logout", 500, body);
        assert_eq!(change, SessionChange::Clear);
        assert_eq!(returned, body);
    }

    #[test]
    fn unauthorized_me_clears_session() {
        let body = br#"{"error":"unauthorized"}"#;
        let (change, returned) = session_change("GET", "/v1/me", 401, body);
        assert_eq!(change, SessionChange::Clear);
        assert_eq!(returned, body);
    }

    #[test]
    fn non_json_response_keeps_session_and_body() {
        let body = b"not json";
        let (change, returned) = session_change("POST", "/v1/auth/login", 200, body);
        assert_eq!(change, SessionChange::Keep);
        assert_eq!(returned, body);
    }
}
