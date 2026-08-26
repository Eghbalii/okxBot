// Package usecase holds application logic (CLAUDE.md §10) — depends only on internal/domain and
// internal/port interfaces, never on concrete adapters, so it's unit-testable without a live
// exchange, database, or RL service.
package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
)

// Trader runs the periodic decide-and-execute loop for a single instrument: fetch state, ask the
// RL service for an action, run it through the risk manager, and execute orders/leverage changes
// on the exchange.
type Trader struct {
	InstID       string
	Exchange     port.ExchangeClient
	Model        port.ModelClient
	RiskManager  *risk.Manager
	PollInterval time.Duration
	TdMode       string // "cross" or "isolated"
	PosMode      string // "net" or "long_short" (hedge mode)
	MinOrderUSD  decimal.Decimal
	Logger       *slog.Logger
}

// Run executes the trading loop until ctx is cancelled.
func (t *Trader) Run(ctx context.Context) error {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(t.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := t.step(ctx, logger); err != nil {
				logger.Error("trading step failed", "instId", t.InstID, "error", err)
			}
		}
	}
}

func (t *Trader) step(ctx context.Context, logger *slog.Logger) error {
	mkt, err := t.Exchange.GetTicker(t.InstID)
	if err != nil {
		return fmt.Errorf("fetch ticker: %w", err)
	}
	mid := mkt.Last

	positions, err := t.Exchange.GetPositions("SWAP")
	if err != nil {
		return fmt.Errorf("fetch positions: %w", err)
	}
	balances, err := t.Exchange.GetBalance("USDT")
	if err != nil {
		return fmt.Errorf("fetch balance: %w", err)
	}

	equity := decimal.Zero
	if len(balances) > 0 {
		equity = balances[0].Eq
	}
	t.RiskManager.CheckDrawdown(equity)
	if halted, reason := t.RiskManager.Halted(); halted {
		logger.Warn("trading halted, skipping step", "reason", reason)
		return nil
	}

	var pos domain.Position
	for _, p := range positions {
		if p.InstID == t.InstID {
			pos = p
			break
		}
	}
	posSize := pos.Pos
	lever := pos.Lever
	uplRatio := pos.UplRatio

	obs := domain.Observation{
		SchemaVersion:    domain.ObservationSchemaVersion,
		InstID:           t.InstID,
		MidPrice:         mid,
		Position:         posSize,
		CurrentLeverage:  lever,
		UnrealizedPnLPct: uplRatio,
		// TODO(CLAUDE.md §15): cmd/trader is not yet part of the per-token paper-trading/RL loop
		// (§14, live wiring still open) — TokenEquityUSD is set to total account equity as a
		// placeholder until this loop is repointed at per-token budgets like the paper-trader path.
		TokenEquityUSD: equity,
	}

	action, err := t.Model.Predict(ctx, obs)
	if err != nil {
		return fmt.Errorf("rl predict: %w", err)
	}

	logger.Info("rl action received", "instId", t.InstID, "targetExposure", action.TargetExposure,
		"leverageFrac", action.LeverageFrac, "confidence", action.Confidence)

	return t.execute(logger, mid, pos, posSize, lever, equity, action)
}

// execute translates the RL agent's action into a leverage change and/or order, after running it
// through the risk manager. It is the only place live orders are placed, so every path that could
// touch real money goes through risk.Manager.Approve first.
func (t *Trader) execute(
	logger *slog.Logger,
	mid decimal.Decimal,
	pos domain.Position,
	posSize, currentLeverage, equity decimal.Decimal,
	action *domain.Action,
) error {
	limits := t.RiskManager.Limits()

	// targetLeverage = 1 + leverageFrac * (maxLeverage - 1)
	targetLeverage := decimal.NewFromInt(1).Add(action.LeverageFrac.Mul(limits.MaxLeverage.Sub(decimal.NewFromInt(1))))

	currentNotional := pos.NotionalUsd
	if currentNotional.IsZero() {
		currentNotional = posSize.Mul(mid)
	}
	// Sign the current notional by position direction so exposure math below works for both
	// long and short starting positions.
	if posSize.IsNegative() {
		currentNotional = currentNotional.Neg()
	}

	targetNotional := action.TargetExposure.Mul(limits.MaxPositionNotionalUSD)

	// Rough, conservative estimate of distance-to-liquidation as a percentage of mark price:
	// ignoring maintenance margin and fees, isolated-margin liquidation occurs at roughly a
	// 1/leverage adverse move. This likely underestimates the true buffer OKX will report (which
	// includes maintenance margin), so it's a conservative floor for the hard-limit check, not an
	// exact liquidation price calculation.
	liqBufferPct := decimal.Zero
	if targetLeverage.IsPositive() {
		liqBufferPct = decimal.NewFromInt(100).Div(targetLeverage)
	}

	approved, err := t.RiskManager.Approve(risk.ProposedAction{
		Leverage:             targetLeverage,
		PositionNotionalUSD:  targetNotional,
		LiquidationBufferPct: liqBufferPct,
	})
	if err != nil {
		logger.Warn("action rejected by risk manager", "instId", t.InstID, "error", err)
		return nil
	}

	if !approved.Leverage.Equal(currentLeverage) && approved.Leverage.IsPositive() {
		req := domain.LeverageChange{InstID: t.InstID, Lever: approved.Leverage, MgnMode: t.TdMode}
		if t.PosMode == "long_short" {
			req.PosSide = posSideFor(approved.PositionNotionalUSD)
		}
		if err := t.Exchange.SetLeverage(req); err != nil {
			return fmt.Errorf("set leverage: %w", err)
		}
		logger.Info("leverage updated", "instId", t.InstID, "leverage", approved.Leverage)
	}

	// risk.Manager.Approve now clamps PositionNotionalUSD by magnitude while preserving sign, so
	// the (possibly clamped) signed target notional is already correct as returned.
	signedTarget := approved.PositionNotionalUSD

	deltaNotional := signedTarget.Sub(currentNotional)
	if !mid.IsPositive() || (t.MinOrderUSD.IsPositive() && deltaNotional.Abs().LessThan(t.MinOrderUSD)) {
		return nil
	}

	side := "buy"
	if deltaNotional.IsNegative() {
		side = "sell"
	}
	// NOTE: sz is computed as underlying USD notional / mid price, i.e. it assumes a contract
	// multiplier of 1. Per-instrument contract-value handling (via /api/v5/public/instruments)
	// is not yet wired in — revisit before trading instruments with a non-1x contract value.
	sz := deltaNotional.Abs().Div(mid)

	order := domain.OrderRequest{
		InstID:  t.InstID,
		TdMode:  t.TdMode,
		Side:    side,
		OrdType: "market",
		Sz:      sz,
	}
	if t.PosMode == "long_short" {
		order.PosSide = posSideFor(signedTarget)
	}

	result, err := t.Exchange.PlaceOrder(order)
	if err != nil {
		return fmt.Errorf("place order: %w", err)
	}
	if result != nil && result.SCode != "0" {
		return fmt.Errorf("order rejected: sCode=%s sMsg=%s", result.SCode, result.SMsg)
	}

	logger.Info("order placed", "instId", t.InstID, "side", side, "sz", sz, "targetNotional", signedTarget)
	return nil
}

// posSideFor maps a signed target notional to OKX's hedge-mode posSide values.
func posSideFor(signedNotional decimal.Decimal) string {
	if signedNotional.IsNegative() {
		return "short"
	}
	return "long"
}
