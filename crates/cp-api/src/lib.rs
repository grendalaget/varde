//! Typed client for the p2pgames control-plane API (agent + public).
//! Types are handwritten to match api/openapi/control-plane.yaml — drift is
//! caught by tests/openapi_drift.rs.

pub mod client;
pub mod sign;
pub mod types;

pub use client::{CpClient, Error, Result};
pub use types::*;
