package api

import (
	"fmt"
	"testing"
)

func TestFailLimiterWindowsAndSweep(t *testing.T) {
	l := newFailLimiter(2, 1000)
	l.Fail(0, "a", "b")
	if !l.Allowed(10, "a") {
		t.Fatal("one failure refused")
	}
	l.Fail(10, "a")
	if l.Allowed(20, "a") || l.Allowed(20, "c", "a") {
		t.Fatal("key at the limit allowed")
	}
	if !l.Allowed(20, "b", "c") {
		t.Fatal("keys under the limit refused")
	}
	if !l.Allowed(1000, "a") {
		t.Fatal("limit outlived its window")
	}

	for i := 0; i < 500; i++ {
		l.Fail(1500, fmt.Sprint("ip-", i))
	}
	l.Fail(3000, "fresh")
	if n := l.size(); n != 1 {
		t.Fatalf("expired windows kept: %d entries, want 1", n)
	}
}
