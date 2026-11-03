//! Typed client for the varde control-plane API (agent + public).
//! Types are handwritten to match api/openapi/control-plane.yaml — drift is
//! caught by tests/openapi_drift.rs.

pub mod client;
pub mod proxy;
pub mod sign;
pub mod sse;
pub mod steam;
pub mod types;

pub use client::{CpClient, Error, Result};
pub use proxy::{session_change, validate_path, ProxyRequest, ProxyResponse, SessionChange};
pub use sse::{SseEvent, SseParser};
pub use steam::{steam_callback_origin, steam_login_url_allowed, steam_navigation, SteamNav};
pub use types::*;
