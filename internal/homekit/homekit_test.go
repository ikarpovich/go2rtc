package homekit

import "testing"

func TestParseAdvertiseIP(t *testing.T) {
	if got := parseAdvertiseIP("192.168.1.10"); got != "192.168.1.10" {
		t.Fatalf("expected IP, got %q", got)
	}
	if got := parseAdvertiseIP("bad-ip"); got != "" {
		t.Fatalf("expected empty for invalid IP, got %q", got)
	}
}

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		raw      string
		min, max int
	}{
		{"30000-30010", 30000, 30010},
		{"30000", 30000, 30000},
		{"", 0, 0},
		{"bad", 0, 0},
		{"0-10", 0, 0},
		{"10-0", 0, 0},
		{"70000-70001", 0, 0},
	}

	for _, tt := range tests {
		min, max := parsePortRange(tt.raw)
		if min != tt.min || max != tt.max {
			t.Fatalf("parsePortRange(%q) = %d-%d, want %d-%d", tt.raw, min, max, tt.min, tt.max)
		}
	}
}
