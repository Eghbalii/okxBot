package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// BTCReference maintains BTC's candle windows for the market-wide observation block
// (docs/RL_V8_PLAN.md), shared by every instrument's engine.
//
// WHY IT IS SEPARATE from the per-instrument engines. Each engine maintains only its own token's
// candles, so an engine trading SOL has no way to see BTC — and every input in the observation was
// intra-token as a result. The operator's own observation is what this fixes: an altcoin can be
// cleanly trending and reverse the moment BTC's candle turns red, and nothing in the model's input
// could express that.
//
// It is also independent of whether BTC is in the TRADED roster. The reference block must be
// present on every observation regardless, so this consumes BTC's candles directly from the event
// bus rather than borrowing a trading engine's window — otherwise disabling BTC for trading would
// silently blind the model about the whole market.
//
// Seeded from Postgres at startup like every other candle window (§14): a fresh process would
// otherwise take ReturnsWindow+1 closed bars before any observation could be built at all, which on
// a 1H bar is half a day of no model calls.
type BTCReference struct {
	// InstID is the symbol BTC is stored under. A constant in practice, but explicit because the
	// symbol changed once already (§33.4's short-symbol migration) and a hardcoded string would
	// have gone quietly wrong rather than failing.
	InstID string

	// Bars is every timeframe to maintain — the same set the engines decide on, since the reference
	// block is built for whichever bar the decision is about.
	Bars         []string
	CandleWindow int

	Repo      port.Repository
	Consumers map[string]port.MarketDataConsumer

	mu      sync.Mutex
	candles map[string][]domain.Candle
}

// Window returns a snapshot of BTC's window for a bar. The bool is false when nothing has been
// collected yet, which callers must treat as "cannot build an observation" rather than substituting
// zeros: a zeroed BTC block reads as "BTC is perfectly flat and uncorrelated", a specific and false
// claim about the market rather than an absence of information.
func (r *BTCReference) Window(bar string) ([]domain.Candle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.candles[bar]
	if !ok || len(w) == 0 {
		return nil, false
	}
	out := make([]domain.Candle, len(w))
	copy(out, w)
	return out, true
}

// Run seeds each bar from the repository and then follows the live feed until ctx is cancelled.
func (r *BTCReference) Run(ctx context.Context, logger *slog.Logger) error {
	r.mu.Lock()
	if r.candles == nil {
		r.candles = make(map[string][]domain.Candle, len(r.Bars))
	}
	r.mu.Unlock()

	// Reuses the engines' own seeding helper, so BTC's window is filled by exactly the same code
	// path (and the same failure handling) as every traded instrument's — one implementation of
	// "read this instrument's recent candles out of Postgres", not a second that can drift.
	seedCandlesFromRepo(ctx, &r.mu, r.candles, r.Repo, r.InstID, r.Bars, r.CandleWindow, logger)

	errCh := make(chan error, len(r.Consumers))
	for bar, c := range r.Consumers {
		bar, c := bar, c
		go func() {
			errCh <- c.Run(ctx, func(ctx context.Context, payload []byte) error {
				return r.handleCandle(bar, payload)
			})
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (r *BTCReference) handleCandle(bar string, payload []byte) error {
	dc, ok, err := decodeCandle(payload, r.InstID)
	if err != nil {
		return fmt.Errorf("btc reference: decode %s candle: %w", bar, err)
	}
	if !ok {
		// A message for another instrument on a shared topic, or a malformed array. Expected, not
		// an error — the dispatcher fans one reader out to every instrument.
		return nil
	}
	// applyCandle takes the mutex itself, and replaces-or-appends by timestamp so the live forming
	// bar is kept current rather than accumulating one entry per push (§15.11).
	applyCandle(&r.mu, r.candles, bar, dc.Candle, r.CandleWindow)
	return nil
}
