package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// SaveFundingRates upserts a batch of settled funding periods (CLAUDE.md, 2026-09-06). Upsert on
// (inst_id, funding_time) since OKX's history endpoint always returns the same recent window —
// re-polling an already-stored period must be a safe no-op, not a duplicate-key error. One
// INSERT per row rather than a multi-row batch: this poller runs hourly against a handful of
// instruments, so there is no real volume here to optimize for.
func (r *Repository) SaveFundingRates(ctx context.Context, rates []port.FundingRate) error {
	for _, fr := range rates {
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO funding_rates (inst_id, funding_time, funding_rate)
			VALUES ($1, $2, $3)
			ON CONFLICT (inst_id, funding_time) DO UPDATE SET funding_rate = EXCLUDED.funding_rate
		`, fr.InstID, fr.FundingTime, fr.FundingRate); err != nil {
			return fmt.Errorf("save funding rate for %s at %s: %w", fr.InstID, fr.FundingTime, err)
		}
	}
	return nil
}

// SumFundingCost sums fundingRate * notionalUSD over every settled period in [openedAt, closedAt]
// for instID — the real funding cost/credit a position of that notional would have paid across its
// lifetime. A positive fundingRate means longs pay shorts (OKX's own convention); realizedPnL is
// responsible for applying the correct sign for the position's own side.
func (r *Repository) SumFundingCost(ctx context.Context, instID string, openedAt, closedAt time.Time, notionalUSD decimal.Decimal) (decimal.Decimal, error) {
	var total *decimal.Decimal
	err := r.pool.QueryRow(ctx, `
		SELECT SUM(funding_rate) FROM funding_rates
		WHERE inst_id = $1 AND funding_time >= $2 AND funding_time <= $3
	`, instID, openedAt, closedAt).Scan(&total)
	if err != nil {
		return decimal.Zero, fmt.Errorf("sum funding cost for %s: %w", instID, err)
	}
	if total == nil {
		return decimal.Zero, nil
	}
	return total.Mul(notionalUSD), nil
}
