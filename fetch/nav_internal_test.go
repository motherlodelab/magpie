package fetch

import (
	"errors"
	"fmt"
	"testing"
)

// TestNavRetryable pins the WaitLoad race classifier: the rod "execution
// context was destroyed" failure (JS redirect mid-load) is the one
// retryable navigation error — budget timeouts and real failures are not.
func TestNavRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"context destroyed", fmt.Errorf("fetch: wait load: %w", errors.New("{-32000 Execution context was destroyed. }")), true},
		{"case-insensitive", errors.New("EXECUTION CONTEXT WAS DESTROYED"), true},
		{"deadline", errors.New("context deadline exceeded"), false},
		{"nil", nil, false},
		{"other rod error", errors.New("{-32000 Target closed}"), false},
	}
	for _, tc := range cases {
		if got := navRetryable(tc.err); got != tc.want {
			t.Errorf("%s: navRetryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
