package rest

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsRetryableError distinguishes "the exchange was busy" from "the exchange said no".
// Retrying a rejected order wastes the rate budget a genuinely transient call needs, and worse,
// repeats a request the exchange has already decided on.
func TestIsRetryableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not retryable", nil, false},
		{"rate limited", &APIError{Code: codeRateLimited}, true},
		{"system busy", &APIError{Code: codeSystemBusy}, true},
		{"internal error", &APIError{Code: codeInternalError}, true},
		{"http 429 without a known code", &APIError{Code: 9999, Status: 429}, true},
		{"parameter error is NOT retryable", &APIError{Code: 600}, false},
		{"insufficient balance is NOT retryable", &APIError{Code: 2005}, false},
		{"transport failure is retryable", errors.New("dial tcp: i/o timeout"), true},
		{"wrapped api error is unwrapped", fmt.Errorf("place order: %w", &APIError{Code: codeRateLimited}), true},
		{"wrapped rejection stays non-retryable", fmt.Errorf("place order: %w", &APIError{Code: 600}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryableError(c.err); got != c.want {
				t.Errorf("IsRetryableError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
