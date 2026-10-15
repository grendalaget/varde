package api_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// A linked machine uploads a panic report (with a minidump); a group admin
// lists and reads it back, a non-member cannot.
func TestCrashReportUploadAndAdminRead(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("pc-1")

	body, _ := json.Marshal(map[string]any{
		"kind":               "panic",
		"exe":                "varde-agent",
		"version":            "0.1.0",
		"pid":                1234,
		"message":            "index out of range",
		"location":           "agent.rs:42:9",
		"backtrace":          "stack...",
		"created_at_unix_ms": e.clk.ms,
		"dump_b64":           base64.StdEncoding.EncodeToString([]byte("MINIDUMP")),
		"dump_name":          "varde-agent.1234.dmp",
	})
	r := a.signedDo("POST", "/v1/agent/crash-reports", body, 0, nil)
	ack := e.mustOK(r)
	id, _ := ack["id"].(string)
	if id == "" {
		t.Fatalf("no report id: %v", ack)
	}

	// a retried upload with the same client_ref dedupes instead of
	// duplicating (lost-ack replay)
	body2, _ := json.Marshal(map[string]any{
		"kind": "panic", "exe": "varde-agent",
		"created_at_unix_ms": e.clk.ms, "client_ref": "stem-1",
	})
	r = a.signedDo("POST", "/v1/agent/crash-reports", body2, 0, nil)
	id2 := e.mustOK(r)["id"].(string)
	r = a.signedDo("POST", "/v1/agent/crash-reports", body2, 0, nil)
	if got := e.mustOK(r)["id"].(string); got != id2 {
		t.Fatalf("replay created a second report: %s vs %s", got, id2)
	}

	l := e.mustOK(e.do("GET", "/v1/groups/"+e.group+"/crash-reports", nil, e.token))
	reps, ok := l["reports"].([]any)
	if !ok || len(reps) != 2 {
		t.Fatalf("want 2 reports (replay deduped), got %v", l)
	}
	var m map[string]any
	for _, x := range reps {
		if x.(map[string]any)["message"] == "index out of range" {
			m = x.(map[string]any)
		}
	}
	if m == nil {
		t.Fatalf("first report missing: %v", l)
	}
	if m["kind"] != "panic" || m["exe"] != "varde-agent" {
		t.Fatalf("bad meta: %v", m)
	}
	if m["dump_size"].(float64) != float64(len("MINIDUMP")) {
		t.Fatalf("bad dump size: %v", m["dump_size"])
	}

	d := e.mustOK(e.do("GET", "/v1/groups/"+e.group+"/crash-reports/"+id, nil, e.token))
	if d["backtrace"] != "stack..." || d["pid"].(float64) != 1234 {
		t.Fatalf("bad detail: %v", d)
	}
	if got, _ := base64.StdEncoding.DecodeString(d["dump_b64"].(string)); string(got) != "MINIDUMP" {
		t.Fatalf("dump did not round-trip")
	}

	// unsigned and non-member access are rejected
	if r := e.do("POST", "/v1/agent/crash-reports", map[string]any{"kind": "panic", "exe": "x", "created_at_unix_ms": 1}, ""); r.Status != 401 {
		t.Fatalf("unsigned: want 401, got %d", r.Status)
	}
	other := e.signup("other@example.com", "password123")
	if r := e.do("GET", "/v1/groups/"+e.group+"/crash-reports", nil, other); r.Status == 200 {
		t.Fatalf("non-member can list crash reports")
	}
	if r := e.do("GET", "/v1/groups/"+e.group+"/crash-reports/nope", nil, e.token); r.Status != 404 {
		t.Fatalf("missing report: want 404, got %d", r.Status)
	}
}
