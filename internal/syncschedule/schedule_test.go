package syncschedule

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantEnabled bool
		want        time.Duration
		wantErr     bool
	}{
		{name: "empty", raw: "", wantEnabled: false},
		{name: "manual", raw: "manual", wantEnabled: false},
		{name: "hourly", raw: "hourly", wantEnabled: true, want: time.Hour},
		{name: "daily", raw: "daily", wantEnabled: true, want: 24 * time.Hour},
		{name: "weekly", raw: "weekly", wantEnabled: true, want: 7 * 24 * time.Hour},
		{name: "every duration", raw: "every 6h", wantEnabled: true, want: 6 * time.Hour},
		{name: "invalid", raw: "sometimes", wantErr: true},
		{name: "invalid duration", raw: "every 0h", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, enabled, err := Parse(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) returned nil error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tt.raw, err)
			}
			if enabled != tt.wantEnabled || got != tt.want {
				t.Fatalf("Parse(%q) = (%v, %v), want (%v, %v)", tt.raw, got, enabled, tt.want, tt.wantEnabled)
			}
		})
	}
}
