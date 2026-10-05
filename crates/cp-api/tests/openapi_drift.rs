//! Drift check: builds a JSON value containing every declared property of
//! each agent schema in api/openapi/control-plane.yaml, deserializes it into
//! the corresponding Rust type, and requires the serialized field names to
//! still match the schema property names.

use std::collections::BTreeSet;

use serde_json::{json, Map, Value};

fn spec() -> Value {
    serde_yaml::from_str(include_str!("../../../api/openapi/control-plane.yaml")).unwrap()
}

fn components() -> Value {
    spec()["components"]["schemas"].clone()
}

fn fake_value(schema: &Value, comps: &Value, depth: usize) -> Value {
    if let Some(r) = schema.get("$ref") {
        let name = r.as_str().unwrap().rsplit('/').next().unwrap();
        return fake_value(&comps[name], comps, depth + 1);
    }
    if depth > 12 {
        return json!({});
    }
    match schema.get("type").and_then(|t| t.as_str()) {
        Some("object") | None => {
            let mut m = Map::new();
            match schema.get("properties") {
                Some(props) => {
                    for (k, v) in props.as_object().unwrap() {
                        m.insert(k.clone(), fake_value(v, comps, depth + 1));
                    }
                }
                None => eprintln!("fake_value: no properties, schema={schema} depth={depth}"),
            }
            Value::Object(m)
        }
        Some("array") => json!([fake_value(&schema["items"], comps, depth + 1)]),
        Some("integer") => json!(1),
        Some("number") => json!(1.5),
        Some("boolean") => json!(true),
        _ => {
            if let Some(e) = schema.get("enum").and_then(|e| e.as_array()) {
                // required fields get the first enum value; others may be null
                return e.first().cloned().unwrap_or(json!("x"));
            }
            json!("x")
        }
    }
}

fn prop_names(schema: &Value, comps: &Value) -> BTreeSet<String> {
    if let Some(r) = schema.get("$ref") {
        let name = r.as_str().unwrap().rsplit('/').next().unwrap();
        return prop_names(&comps[name], comps);
    }
    schema
        .get("properties")
        .and_then(|p| p.as_object())
        .map(|p| p.keys().cloned().collect())
        .unwrap_or_default()
}

/// Deserialize `v` into T, then compare the property-name sets.
fn check<T: serde::de::DeserializeOwned + serde::Serialize>(schema_name: &str) {
    let comps = components();
    let schema = &comps[schema_name];
    let fake = fake_value(schema, &comps, 0);
    let parsed: T = serde_json::from_value(fake.clone())
        .unwrap_or_else(|e| panic!("{schema_name}: {e} on {fake}"));
    let back = serde_json::to_value(&parsed).unwrap();
    let want = prop_names(schema, &comps);
    let got: BTreeSet<String> = back
        .as_object()
        .map(|m| m.keys().cloned().collect())
        .unwrap_or_default();
    // every schema property the server can send must be representable
    for w in &want {
        if !got.contains(w) && fake.get(w).map(|v| !v.is_null()).unwrap_or(false) {
            panic!("{schema_name}: field {w} dropped or renamed");
        }
    }
}

#[test]
fn agent_schemas_in_sync() {
    check::<cp_api::AgentHeartbeat>("AgentHeartbeat");
    check::<cp_api::AgentDirectives>("AgentDirectives");
    check::<cp_api::ExecutionDirective>("ExecutionDirective");
    check::<cp_api::DirectivePeer>("DirectivePeer");
    check::<cp_api::DirectiveRoute>("DirectiveRoute");
    check::<cp_api::DirectiveRelay>("DirectiveRelay");
    check::<cp_api::ReplicationTask>("ReplicationTask");
    check::<cp_api::SnapshotRequest>("SnapshotRequest");
    check::<cp_api::ExecutionReport>("ExecutionReport");
    check::<cp_api::Capabilities>("Capabilities");
    check::<cp_api::EnrollResult>("EnrollResult");
    check::<cp_api::DeviceEnrollResponse>("DeviceEnrollResponse");
    check::<cp_api::Service>("Service");
}

/// Request-side types must accept (and reserialize) every declared field.
#[test]
fn request_schemas_in_sync() {
    check::<cp_api::TokenEnrollRequest>("TokenEnrollRequest");
    check::<cp_api::DeviceEnrollRequest>("DeviceEnrollRequest");
    check::<cp_api::AgentSnapshot>("AgentSnapshot");

    // The execution-status body is inline in the yaml (no named schema) —
    // check the Rust type against the declared field set by hand.
    let update: cp_api::ExecutionStatusUpdate = serde_json::from_value(serde_json::json!(
        {"server_id":"s","epoch":1,"state":"running","health":"ok","message":"m"}
    ))
    .unwrap();
    let back = serde_json::to_value(&update).unwrap();
    for f in ["server_id", "epoch", "state", "health", "message"] {
        assert!(back.get(f).is_some(), "status update missing {f}");
    }
}
