package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errRateLimited = errors.New("okx api error: code=50011 msg=rate limit reached")
var errInsufficientBalance = errors.New("okx api error: code=51008 msg=insufficient balance")

func isRateLimitErr(err error) bool { return errors.Is(err, errRateLimited) }

func TestRetryPolicy_SucceedsWithoutRetryOnFirstSuccess(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	calls := 0
	err := p.Do(context.Background(), isRateLimitErr, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call, got %d", calls)
	}
}

func TestRetryPolicy_RetriesOnRateLimitThenSucceeds(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	calls := 0
	err := p.Do(context.Background(), isRateLimitErr, func() error {
		calls++
		if calls < 3 {
			return errRateLimited
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (2 failures + 1 success), got %d", calls)
	}
}

func TestRetryPolicy_DoesNotRetryNonRetryableError(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	calls := 0
	err := p.Do(context.Background(), isRateLimitErr, func() error {
		calls++
		return errInsufficientBalance
	})
	if !errors.Is(err, errInsufficientBalance) {
		t.Fatalf("expected errInsufficientBalance, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call (no retry on non-retryable error), got %d", calls)
	}
}

func TestRetryPolicy_GivesUpAfterMaxAttempts(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	calls := 0
	err := p.Do(context.Background(), isRateLimitErr, func() error {
		calls++
		return errRateLimited
	})
	if !errors.Is(err, errRateLimited) {
		t.Fatalf("expected errRateLimited, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly MaxAttempts=3 calls, got %d", calls)
	}
}

func TestRetryPolicy_RespectsContextCancellationDuringBackoff(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, BaseDelay: time.Hour, MaxDelay: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	err := p.Do(ctx, isRateLimitErr, func() error {
		calls++
		return errRateLimited
	})
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call before the long backoff was interrupted, got %d", calls)
	}
}

func TestDefaultRetryPolicy_HasSaneValues(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.MaxAttempts <= 1 {
		t.Fatalf("MaxAttempts should allow at least one retry, got %d", p.MaxAttempts)
	}
	if p.BaseDelay <= 0 || p.MaxDelay <= 0 {
		t.Fatalf("delays must be positive: base=%v max=%v", p.BaseDelay, p.MaxDelay)
	}
	if p.BaseDelay > p.MaxDelay {
		t.Fatalf("BaseDelay (%v) should not exceed MaxDelay (%v)", p.BaseDelay, p.MaxDelay)
	}
}
