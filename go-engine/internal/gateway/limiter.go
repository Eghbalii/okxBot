// Package gateway holds the pure, dependency-free rate-limiting/priority logic behind
// cmd/okx-gateway (CLAUDE.md §27.1) — kept separate from the HTTP-plumbing-heavy service code so
// the actual admission decision (does this request get a token now, or wait) is unit-testable
// without a real HTTP server, matching this repo's existing pattern (e.g. internal/optimizer's
// scoring.go, internal/usecase's RatchetSLTP).
//
// Every other service that talks to OKX (paper-trader, ingestor, strategy-tester,
// strategy-optimizer) currently builds its own independent rest.Client, each enforcing only a
// local, non-endpoint-aware concurrency cap (rest.Client's maxConcurrentRequests=3) — nothing
// coordinates the actual OKX-side rate-limit budget across processes. This package is the
// coordination point: real trading (cmd/trader) must never be delayed by a burst of
// paper-trading/optimizer traffic, so admission is decided per (consumer, endpoint class) token
// bucket, with the trader consumer's bucket for trade-critical classes always checked and
// refilled ahead of any other consumer contending for the same OKX-side limit.
package gateway

import (
	"context"
	"sync"
	"time"
)

// EndpointClass groups OKX REST endpoints that share one rate-limit budget on OKX's side
// (CLAUDE.md §27.1 — OKX limits are defined per endpoint, not in aggregate; trading endpoints
// (place/cancel/amend/leverage) are limited independently from account-read endpoints, which are
// independent again from public market-data endpoints). Exact OKX numbers must be verified
// against the live docs before deploying — see NewDefaultLimits' own warning.
type EndpointClass string

const (
	ClassTrade    EndpointClass = "trade"    // place/cancel/amend order
	ClassLeverage EndpointClass = "leverage" // set-leverage
	ClassAccount  EndpointClass = "account"  // positions, balance, order status
	ClassMarket   EndpointClass = "market"   // tickers, candles, history-candles, instruments
)

// ConsumerPriority orders which consumer's queued request is admitted first when two consumers
// are both waiting on the same endpoint class at the same instant. Lower value = higher priority.
// PriorityTrader is reserved for cmd/trader specifically (CLAUDE.md §27.1's explicit "the
// trader's requests get strict priority over every other consumer's requests").
type ConsumerPriority int

const (
	PriorityTrader ConsumerPriority = 0
	PriorityNormal ConsumerPriority = 1
)

// ClassLimit is one endpoint class's token-bucket shape: Capacity tokens, refilled at Refill
// tokens per Interval (a standard token-bucket, not a fixed window — avoids the thundering-herd
// refill-boundary burst a naive fixed-window counter allows).
type ClassLimit struct {
	Capacity int
	Refill   int
	Interval time.Duration
}

// DefaultLimits returns a conservative starting point for each endpoint class.
//
// THESE NUMBERS ARE NOT VERIFIED AGAINST OKX'S LIVE DOCS (CLAUDE.md §27.1) — third-party sources
// consulted while designing this disagreed with each other, and the official per-endpoint rate
// limit table did not fetch cleanly through available tooling while writing this. Re-check
// https://www.okx.com/docs-v5/en/#overview-rate-limits and each endpoint's own page before
// deploying against real trading, and prefer overriding via config (these are only the
// code-level fallback if config supplies nothing) rather than editing this function — a
// correction after checking the real docs, or a VIP-tier fill-ratio change (OKX's limits scale
// with account tier), should never need a code change.
func DefaultLimits() map[EndpointClass]ClassLimit {
	return map[EndpointClass]ClassLimit{
		// Trading endpoints: kept well below every third-party figure seen (all clustered around
		// 60/2s per instrument) so a wrong number errs toward being too conservative, not too
		// permissive, for the class that places real orders.
		ClassTrade: {Capacity: 20, Refill: 20, Interval: 2 * time.Second},
		// Leverage changes are infrequent by nature (only when the model's target leverage
		// actually differs from current) — a tight bucket here costs nothing.
		ClassLeverage: {Capacity: 10, Refill: 10, Interval: 2 * time.Second},
		// Position/balance/order-status reads: polled every Trading.PollIntervalSec by cmd/trader
		// alone, plus periodic reconciliation reads (CLAUDE.md §27.6) — kept comfortably above
		// that cadence.
		ClassAccount: {Capacity: 10, Refill: 10, Interval: 2 * time.Second},
		// Market data (candles/tickers/instruments) is the highest-volume, least risk-sensitive
		// class — multiple services seeding candle windows concurrently once caused a real
		// incident (CLAUDE.md §14's history-candles concurrency bug; that endpoint/feature has
		// since been removed entirely per explicit operator instruction, but the class sizing
		// rationale still applies to whatever market-data traffic exists), so this bucket is sized
		// to absorb bursts without starving the trade-critical classes above, which have entirely
		// separate buckets and therefore cannot be starved BY market-data traffic regardless of
		// this number.
		ClassMarket: {Capacity: 20, Refill: 20, Interval: 2 * time.Second},
	}
}

// bucket is a single token bucket for one (consumer, class) pair.
type bucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	refillPS float64 // tokens per second
	last     time.Time
}

func newBucket(limit ClassLimit, now time.Time) *bucket {
	return &bucket{
		tokens:   float64(limit.Capacity),
		capacity: float64(limit.Capacity),
		refillPS: float64(limit.Refill) / limit.Interval.Seconds(),
		last:     now,
	}
}

func (b *bucket) refillLocked(now time.Time) {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.refillPS
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
}

// tryTake attempts to remove one token, returning whether it succeeded and (if not) how long
// until a token will next be available.
func (b *bucket) tryTake(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	if b.refillPS <= 0 {
		return false, time.Second // degenerate config; avoid divide-by-zero busy-loop
	}
	need := 1 - b.tokens
	wait := time.Duration(need/b.refillPS*float64(time.Second)) + time.Millisecond
	return false, wait
}

// Limiter is the gateway's admission point: one call to Acquire per outbound OKX REST request,
// blocking (respecting ctx) until a token is available for that (consumer, class) pair AND — this
// is the priority mechanism — no higher-priority consumer is currently also waiting on the same
// class. A lower-priority request that finds tokens available immediately still proceeds
// immediately; priority only matters under contention, so normal (non-trader) traffic is never
// penalized when the trader isn't active.
type Limiter struct {
	mu      sync.Mutex
	buckets map[EndpointClass]map[string]*bucket // class -> consumer -> bucket
	limits  map[EndpointClass]ClassLimit
	// waiting counts, per class, how many PriorityTrader Acquire calls are currently blocked on
	// that class's own bucket — checked by every PriorityNormal Acquire loop so normal traffic
	// backs off while the trader is contending, rather than racing it on each bucket refill tick.
	waiting  map[EndpointClass]int
	now      func() time.Time
	pollTick time.Duration
}

// NewLimiter builds a Limiter from the given per-class limits (use DefaultLimits() for the
// code-level fallback, or a config-supplied map — see CLAUDE.md §27.1's "config values, not
// hardcoded constants" requirement).
func NewLimiter(limits map[EndpointClass]ClassLimit) *Limiter {
	return &Limiter{
		buckets:  make(map[EndpointClass]map[string]*bucket),
		limits:   limits,
		waiting:  make(map[EndpointClass]int),
		now:      time.Now,
		pollTick: 5 * time.Millisecond,
	}
}

func (l *Limiter) bucketFor(class EndpointClass, consumer string) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.buckets[class]
	if !ok {
		m = make(map[string]*bucket)
		l.buckets[class] = m
	}
	b, ok := m[consumer]
	if !ok {
		limit, ok := l.limits[class]
		if !ok {
			limit = ClassLimit{Capacity: 1, Refill: 1, Interval: time.Second}
		}
		b = newBucket(limit, l.now())
		m[consumer] = b
	}
	return b
}

// Acquire blocks until a token is available for (class, consumer), or ctx is done. priority
// governs queueing order under contention: while any PriorityTrader request is waiting on this
// class, a PriorityNormal request backs off an extra tick before re-checking its own bucket, so a
// burst of low-priority traffic cannot keep re-winning a race against the trader's own retries.
// This is a soft/best-effort priority (no hard preemption of an already-in-flight request — HTTP
// requests can't be preempted once sent), which is sufficient here: OKX requests are short-lived
// (rest.Client's 10s timeout), so "admit the trader's next request first" bounds the trader's
// added latency to at most one other request's round trip, not an unbounded queue.
func (l *Limiter) Acquire(ctx context.Context, class EndpointClass, consumer string, priority ConsumerPriority) error {
	b := l.bucketFor(class, consumer)

	if priority == PriorityTrader {
		l.mu.Lock()
		l.waiting[class]++
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			l.waiting[class]--
			l.mu.Unlock()
		}()
	}

	for {
		if priority != PriorityTrader {
			l.mu.Lock()
			traderWaiting := l.waiting[class] > 0
			l.mu.Unlock()
			if traderWaiting {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(l.pollTick):
				}
				continue
			}
		}

		ok, wait := b.tryTake(l.now())
		if ok {
			return nil
		}
		if wait > l.pollTick {
			wait = l.pollTick
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
