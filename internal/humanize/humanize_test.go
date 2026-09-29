package humanize

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{0, "0s"},
		{45 * time.Second, "45s"},
		{59*time.Second + 600*time.Millisecond, "1m"},
		{60 * time.Second, "1m"},
		{12*time.Minute + 5*time.Second, "12m5s"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 13*time.Minute + 40*time.Second, "2h13m"},
		{24 * time.Hour, "1d"},
		{3*24*time.Hour + 4*time.Hour + 30*time.Minute, "3d4h"},
	}
	for _, tt := range tests {
		if got := Duration(tt.in); got != tt.want {
			t.Errorf("Duration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
