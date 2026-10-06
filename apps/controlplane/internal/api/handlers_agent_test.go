package api

import (
	"reflect"
	"testing"
)

func TestFilterDialEndpoints(t *testing.T) {
	input := []string{
		"[::]:41641",
		"0.0.0.0:41641",
		"::",
		"127.0.0.1:41641",
		"mesh.example:41641",
	}
	want := []string{"127.0.0.1:41641", "mesh.example:41641"}
	if got := filterDialEndpoints(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("filterDialEndpoints()=%v, want %v", got, want)
	}
}
