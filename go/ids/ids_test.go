package ids

import (
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	id, err := New(Server)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(id, Server) {
		t.Fatalf("id %q missing prefix %q", id, Server)
	}
	if got := len(id) - len(Server); got != 20 {
		t.Fatalf("random part length = %d, want 20", got)
	}
	for _, c := range id[len(Server):] {
		if !strings.ContainsRune(alphabet, c) {
			t.Fatalf("id %q contains invalid char %q", id, c)
		}
	}
}

func TestNewUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := Must(User)
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
