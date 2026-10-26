package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (e *env) emitEvents(n int, typ string) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		if err := e.st.EmitEvent(context.Background(), e.st.DB, e.group, nil, nil, typ, map[string]any{"i": i}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) listEvents(query string) []map[string]any {
	e.t.Helper()
	b := e.mustOK(e.do("GET", "/v1/groups/"+e.group+"/events"+query, nil, e.token))
	var out []map[string]any
	for _, v := range b["events"].([]any) {
		out = append(out, v.(map[string]any))
	}
	return out
}

// Heartbeats re-report running every interval; only the transition is an event.
func TestRunningHeartbeatsEmitOneStateEvent(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("node-a")
	srv := e.createServer("testgame", "s1", nil)
	host, execID, epoch := e.startToRunning(srv, a)
	report := []map[string]any{{
		"execution_id": execID, "server_id": srv, "epoch": epoch, "state": "running",
	}}
	for i := 0; i < 3; i++ {
		e.clk.advance(5000)
		if _, r := host.heartbeat(report); r.Status != 200 {
			t.Fatalf("heartbeat: %d %s", r.Status, r.Raw)
		}
	}
	var n int
	if err := e.st.DB.Get(&n, e.st.Rebind(
		`SELECT COUNT(*) FROM events WHERE type='execution.state' AND server_id=?`), srv); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("execution.state events = %d, want 1", n)
	}
}

func TestListEventsDefaultsToNewest(t *testing.T) {
	e := newEnv(t)
	e.emitEvents(600, "test.event")
	maxID, err := e.st.MaxEventID(context.Background(), e.group)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(evs []map[string]any) []int64 {
		out := make([]int64, len(evs))
		for i, ev := range evs {
			out[i] = int64(ev["id"].(float64))
		}
		return out
	}

	got := ids(e.listEvents(""))
	if len(got) != 200 || got[len(got)-1] != maxID || got[0] != maxID-199 {
		t.Fatalf("default page: %d events, first %d last %d (max %d)", len(got), got[0], got[len(got)-1], maxID)
	}
	if got := e.listEvents("?limit=0"); len(got) != 200 {
		t.Fatalf("limit=0 returned %d, want 200", len(got))
	}
	if got := e.listEvents("?limit=10000"); len(got) != 500 {
		t.Fatalf("limit=10000 returned %d, want the 500 cap", len(got))
	}
	// with a cursor, page forward from it
	after := ids(e.listEvents("?after=0&limit=5"))
	if len(after) != 5 || after[0] >= after[4] || after[4] > maxID-500 {
		t.Fatalf("after=0 page: %v", after)
	}
}

type sseStream struct {
	lines <-chan string
	close func()
}

func (e *env) openSSE(lastEventID string) sseStream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.http.URL+"/v1/groups/"+e.group+"/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		cancel()
		e.t.Fatalf("stream status %d", resp.StatusCode)
	}
	lines := make(chan string, 1024)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				lines <- line
			}
		}
	}()
	s := sseStream{lines: lines, close: func() { cancel(); _ = resp.Body.Close() }}
	e.t.Cleanup(s.close)
	return s
}

func (s sseStream) next(t *testing.T, within time.Duration) map[string]any {
	t.Helper()
	select {
	case line, ok := <-s.lines:
		if !ok {
			t.Fatal("stream closed")
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		return ev
	case <-time.After(within):
		t.Fatalf("no event within %s", within)
	}
	return nil
}

// A fresh stream starts at the newest event (no history replay) and wakes on
// the reconciler's change notification instead of polling.
func TestSSEStartsAtNewestAndWakesOnChange(t *testing.T) {
	e := newEnv(t)
	e.emitEvents(3, "test.old")
	e.reconcile() // record the group fingerprint
	stream := e.openSSE("")

	time.Sleep(100 * time.Millisecond)
	e.emitEvents(1, "test.new")
	e.reconcile() // fingerprint (max event id) changed → notify the group
	if ev := stream.next(t, 2*time.Second); ev["type"] != "test.new" {
		t.Fatalf("first streamed event = %v, want test.new (no history replay)", ev["type"])
	}

	// a reconnect resumes after Last-Event-ID
	oldest := e.listEvents("?after=0&limit=1")[0]["id"].(float64)
	resumed := e.openSSE(fmt.Sprint(int64(oldest)))
	if ev := resumed.next(t, 2*time.Second); ev["id"].(float64) != oldest+1 {
		t.Fatalf("resumed stream started at %v, want %v", ev["id"], oldest+1)
	}
}
