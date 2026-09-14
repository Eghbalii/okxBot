package usecase

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// TokenStatsCache supplies the roster-wide half of the observation's token profile
// (docs/RL_V8_PLAN.md) — volume, its rank among the scanned market, and the 24h figures.
//
// WHY A CACHE rather than a query per decision: these are roster-wide facts that change on the
// discovery scan's own cadence (hours, §53), while decisions happen on every tick. Reading the
// database per decision would issue thousands of identical queries an hour for a number that has
// not moved, and — worse — would put a database round trip on the path a trading decision waits on.
//
// Stale is the correct failure mode here. A profile a few hours old still describes the instrument
// accurately enough for "is this a thin, volatile market or a deep, calm one", which is the only
// question these inputs answer. A missing profile leaves the fields zero, which is honest: an
// unranked token of unknown volume.
type TokenStatsCache struct {
	Repo    port.Repository
	Refresh time.Duration

	mu    sync.RWMutex
	stats map[string]domain.TokenProfile
}

// For returns instID's profile, or a zero profile if the token is not in the scanned snapshot.
func (c *TokenStatsCache) For(instID string) domain.TokenProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats[instID]
}

// Run loads once immediately and then refreshes on an interval until ctx is cancelled.
func (c *TokenStatsCache) Run(ctx context.Context, logger *slog.Logger) error {
	interval := c.Refresh
	if interval <= 0 {
		interval = time.Hour
	}
	c.reload(ctx, logger)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			c.reload(ctx, logger)
		}
	}
}

func (c *TokenStatsCache) reload(ctx context.Context, logger *slog.Logger) {
	toks, err := c.Repo.ListMarketTokens(ctx, "", 0)
	if err != nil {
		// Keeps whatever was loaded before rather than clearing: an empty profile would tell the
		// model every token is unranked with zero volume, which is a claim, where stale data is
		// merely slightly out of date.
		logger.Warn("token stats: reload failed, keeping the previous snapshot", "error", err)
		return
	}

	// Rank by volume across the whole scanned market, not just the traded roster: the question the
	// model is being asked is "how deep is this market relative to what exists", and ranking within
	// a hand-picked roster of ten would make the smallest of them look like the bottom of the
	// market when it may be the 30th-largest instrument on the exchange.
	best := decimal.Zero
	for _, t := range toks {
		if t.Vol24hUSD.GreaterThan(best) {
			best = t.Vol24hUSD
		}
	}

	next := make(map[string]domain.TokenProfile, len(toks))
	for _, t := range toks {
		p := domain.TokenProfile{
			Range24h:  t.Range24hPct,
			Change24h: t.Change24hPct,
		}
		if t.Vol24hUSD.IsPositive() {
			f, _ := t.Vol24hUSD.Float64()
			if f > 0 {
				p.LogVolume24h = decimal.NewFromFloat(math.Log10(f))
			}
			if best.IsPositive() {
				p.VolumeRank = t.Vol24hUSD.Div(best)
			}
		}
		// A symbol can appear once per exchange (§46); keep the deepest venue's figures, matching
		// how the panel rolls a token up — averaging a thin venue against a deep one produces a
		// number that exists nowhere.
		if prev, ok := next[t.Symbol]; ok && prev.LogVolume24h.GreaterThan(p.LogVolume24h) {
			continue
		}
		next[t.Symbol] = p
	}

	c.mu.Lock()
	c.stats = next
	c.mu.Unlock()
	logger.Info("token stats loaded", "tokens", len(next))
}
