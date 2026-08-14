package kiln

import (
	"errors"
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

func TestFormatServiceExit(t *testing.T) {
	tests := []struct {
		name     string
		code     *int
		err      error
		lifetime time.Duration
		want     string
	}{
		{
			name:     "the motivating case: a dev server that died seven minutes in",
			code:     intPtr(1),
			err:      errors.New("exit status 1"),
			lifetime: 7*time.Minute + 31*time.Second,
			want:     "exited (exit 1, lived 7m31s)",
		},
		{
			// A clean exit is still a death: a service that returns 0 is as gone
			// as one that crashes, and the panel must not read healthy either way.
			name:     "a clean exit",
			code:     intPtr(0),
			lifetime: 45 * time.Second,
			want:     "exited (exit 0, lived 45s)",
		},
		{
			// A signalled process has no exit status, so the cause is the signal
			// rather than an invented -1 that would read as a chosen code.
			name:     "killed by a signal",
			err:      errors.New("signal: killed"),
			lifetime: 2*time.Hour + 5*time.Minute,
			want:     "exited (signal: killed, lived 2h05m)",
		},
		{
			name: "nothing measured drops the lifetime rather than inventing 0s",
			code: intPtr(3),
			want: "exited (exit 3)",
		},
		{
			name:     "no code and no cause",
			lifetime: 90 * time.Second,
			want:     "exited (exited, lived 1m30s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatServiceExit(tt.code, tt.err, tt.lifetime); got != tt.want {
				t.Errorf("FormatServiceExit = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatLifetime(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"},
		{999 * time.Millisecond, "1s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m00s"},
		{7*time.Minute + 31*time.Second, "7m31s"},
		{time.Hour, "1h00m"},
		{25*time.Hour + 30*time.Minute, "25h30m"},
	}
	for _, tt := range tests {
		if got := FormatLifetime(tt.in); got != tt.want {
			t.Errorf("FormatLifetime(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
