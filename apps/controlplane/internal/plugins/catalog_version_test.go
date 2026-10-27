package plugins

import "testing"

func TestIsVersionNewer(t *testing.T) {
	tests := []struct {
		candidate string
		current   string
		want      bool
	}{
		{candidate: "1.2.0", current: "1.1.9", want: true},
		{candidate: "v1.0.0", current: "1.0.0-rc.1", want: true},
		{candidate: "1.0.0-rc.10", current: "1.0.0-rc.2", want: true},
		{candidate: "1.0.0-rc.1", current: "1.0.0", want: false},
		{candidate: "1.0.0+build.2", current: "1.0.0+build.1", want: false},
		{candidate: "1.0", current: "0.9.0", want: false},
		{candidate: "1.0.0", current: "2.0.0", want: false},
	}
	for _, test := range tests {
		t.Run(test.candidate+"_over_"+test.current, func(t *testing.T) {
			if got := IsVersionNewer(test.candidate, test.current); got != test.want {
				t.Fatalf("IsVersionNewer(%q, %q) = %v, want %v", test.candidate, test.current, got, test.want)
			}
		})
	}
}
