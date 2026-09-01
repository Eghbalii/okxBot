package gateway

import (
	"context"
	"math/rand"
	"time"
)

// RetryPolicy is exponential backoff with jitter for OKX 429/rate-limit-class errors (CLAUDE.md
// §27.1 — "does not exist anywhere in the codebase today"; rest.Client.do() returns the first
// error immediately). Centralized here rather than in each of the five services that used to call
// OKX directly, since the gateway is now the only process making the actual HTTP call.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryPolicy is a conservative default: a handful of attempts, capped backoff, so a
// persistent OKX-side outage fails fast rather than retrying indefinitely and masking the
// problem from the caller.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, BaseDelay: 200 * time.Millisecond, MaxDelay: 5 * time.Second}
}

// delay returns the backoff before attempt N (0-indexed: attempt 0 is the first retry after the
// initial call), full jitter between 0 and the exponential cap — full jitter (not
// capped-exponential-then-jitter) is the AWS-architecture-blog-recommended shape for avoiding
// synchronized retry storms across multiple callers hitting the same rate limit simultaneously,
// which is exactly this gateway's situation (multiple consumers, one shared OKX-side budget).
func (p RetryPolicy) delay(attempt int, rnd *rand.Rand) time.Duration {
	ceiling := p.BaseDelay << attempt
	if ceiling > p.MaxDelay || ceiling <= 0 { // overflow guard: shifting far enough left wraps negative
		ceiling = p.MaxDelay
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rnd.Int63n(int64(ceiling)))
}

// RetryableError reports whether err represents an OKX rate-limit response worth retrying (as
// opposed to e.g. a validation error like insufficient balance, which retrying can never fix).
// IsRateLimit is injected rather than hardcoded to OKX's error-string shape so this file stays
// testable without constructing a real OKX error envelope.
type RetryableError func(err error) bool

// Do runs fn, retrying up to MaxAttempts-1 additional times (full jitter backoff between
// attempts) while isRetryable(err) is true and ctx is not done. Returns the last error if every
// attempt fails, or nil on the first success.
func (p RetryPolicy) Do(ctx context.Context, isRetryable RetryableError, fn func() error) error {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	var lastErr error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		if attempt > 0 {
			d := p.delay(attempt-1, rnd)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
	}
	return lastErr
}
