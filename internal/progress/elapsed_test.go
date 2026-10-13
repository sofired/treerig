package progress

import (
	"testing"
	"time"
)

func TestFormatElapsed(t *testing.T) {
	for _, tc := range []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"floors zero at 1s", 0, "1s"},
		{"floors a sub-second duration at 1s", 500 * time.Millisecond, "1s"},
		{"whole seconds under a minute", 59 * time.Second, "59s"},
		{"exactly one minute", 60 * time.Second, "1m 00s"},
		{"minutes with zero-padded seconds", 65 * time.Second, "1m 05s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatElapsed(tc.ago); got != tc.want {
				t.Fatalf("formatElapsed(%v) = %q, want %q", tc.ago, got, tc.want)
			}
		})
	}
}
