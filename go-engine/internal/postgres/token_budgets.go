package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// GetTokenBudget returns instID's current budget row, seeding it at initialBudgetUSD if it
// doesn't exist yet (CLAUDE.md §15.7).
func (r *Repository) GetTokenBudget(ctx context.Context, instID string, initialBudgetUSD decimal.Decimal) (port.TokenBudget, error) {
	var tb port.TokenBudget
	err := r.pool.QueryRow(ctx, `
		INSERT INTO token_budgets (inst_id, budget_usd, equity_usd)
		VALUES ($1, $2, $2)
		ON CONFLICT (inst_id) DO UPDATE SET inst_id = token_budgets.inst_id
		RETURNING inst_id, budget_usd, equity_usd, reset_count, last_reset_at, updated_at
	`, instID, initialBudgetUSD).Scan(&tb.InstID, &tb.BudgetUSD, &tb.EquityUSD, &tb.ResetCount, &tb.LastResetAt, &tb.UpdatedAt)
	if err != nil {
		return port.TokenBudget{}, fmt.Errorf("get token budget for %s: %w", instID, err)
	}
	return tb, nil
}

// ApplyTokenPnL adds pnl to instID's running equity, resetting it back to its configured
// BudgetUSD (and recording the reset) if the result is <= 0 — CLAUDE.md §15.7's "give it another
// chance." This is paper/demo-mode bookkeeping only; a real-money path must never call this.
func (r *Repository) ApplyTokenPnL(ctx context.Context, instID string, pnl decimal.Decimal) (port.TokenBudget, bool, error) {
	var tb port.TokenBudget
	err := r.pool.QueryRow(ctx, `
		UPDATE token_budgets
		SET equity_usd = equity_usd + $2,
			updated_at = now()
		WHERE inst_id = $1
		RETURNING inst_id, budget_usd, equity_usd, reset_count, last_reset_at, updated_at
	`, instID, pnl).Scan(&tb.InstID, &tb.BudgetUSD, &tb.EquityUSD, &tb.ResetCount, &tb.LastResetAt, &tb.UpdatedAt)
	if err != nil {
		return port.TokenBudget{}, false, fmt.Errorf("apply pnl to token budget for %s: %w", instID, err)
	}

	if tb.EquityUSD.Sign() > 0 {
		return tb, false, nil
	}

	err = r.pool.QueryRow(ctx, `
		UPDATE token_budgets
		SET equity_usd = budget_usd,
			reset_count = reset_count + 1,
			last_reset_at = now(),
			updated_at = now()
		WHERE inst_id = $1
		RETURNING inst_id, budget_usd, equity_usd, reset_count, last_reset_at, updated_at
	`, instID).Scan(&tb.InstID, &tb.BudgetUSD, &tb.EquityUSD, &tb.ResetCount, &tb.LastResetAt, &tb.UpdatedAt)
	if err != nil {
		return port.TokenBudget{}, false, fmt.Errorf("reset drained token budget for %s: %w", instID, err)
	}
	return tb, true, nil
}
