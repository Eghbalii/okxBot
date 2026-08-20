// Package engine orchestrates the live trading loop: fetch state, ask the RL service for an
// action, run it through the risk manager, and execute orders/leverage changes on OKX.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/rez/okxBot/go-engine/internal/okx"
	"github.com/rez/okxBot/go-engine/internal/okx/rest"
	"github.com/rez/okxBot/go-engine/internal/risk"
	"github.com/rez/okxBot/go-engine/internal/rlclient"
)

// Trader runs the periodic decide-and-execute loop for a single instrument.
type Trader struct {
	InstID       string
	RESTClient   *rest.Client
	RLClient     *rlclient.Client
	RiskManager  *risk.Manager
	PollInterval time.Duration
	TdMode       string // "cross" or "isolated"
	PosMode      string // "net" or "long_short" (hedge mode)
	MinOrderUSD  float64
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
	mkt, err := t.RESTClient.GetTicker(t.InstID)
	if err != nil {
		return fmt.Errorf("fetch ticker: %w", err)
	}
	mid, err := strconv.ParseFloat(mkt.Last, 64)
	if err != nil {
		return fmt.Errorf("parse last price %q: %w", mkt.Last, err)
	}

	positions, err := t.RESTClient.GetPositions("SWAP")
	if err != nil {
		return fmt.Errorf("fetch positions: %w", err)
	}
	balances, err := t.RESTClient.GetBalance("USDT")
	if err != nil {
		return fmt.Errorf("fetch balance: %w", err)
	}

	var equity float64
	if len(balances) > 0 {
		equity, _ = strconv.ParseFloat(balances[0].Eq, 64)
	}
	t.RiskManager.CheckDrawdown(equity)
	if halted, reason := t.RiskManager.Halted(); halted {
		logger.Warn("trading halted, skipping step", "reason", reason)
		return nil
	}

	var pos okx.Position
	for _, p := range positions {
		if p.InstID == t.InstID {
			pos = p
			break
		}
	}
	posSize, _ := strconv.ParseFloat(pos.Pos, 64)
	lever, _ := strconv.ParseFloat(pos.Lever, 64)
	uplRatio, _ := strconv.ParseFloat(pos.UplRatio, 64)

	obs := rlclient.Observation{
		InstID:           t.InstID,
		MidPrice:         mid,
		Position:         posSize,
		CurrentLeverage:  lever,
		UnrealizedPnLPct: uplRatio,
		EquityUSD:        equity,
	}

	action, err := t.RLClient.Predict(ctx, obs)
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
	mid float64,
	pos okx.Position,
	posSize, currentLeverage, equity float64,
	action *rlclient.Action,
) error {
	limits := t.RiskManager.Limits()

	targetLeverage := 1 + action.LeverageFrac*(limits.MaxLeverage-1)

	currentNotional, _ := strconv.ParseFloat(pos.NotionalUsd, 64)
	if currentNotional == 0 {
		currentNotional = posSize * mid
	}
	// Sign the current notional by position direction so exposure math below works for both
	// long and short starting positions.
	if posSize < 0 {
		currentNotional = -currentNotional
	}

	targetNotional := action.TargetExposure * limits.MaxPositionNotionalUSD

	// Rough, conservative estimate of distance-to-liquidation as a percentage of mark price:
	// ignoring maintenance margin and fees, isolated-margin liquidation occurs at roughly a
	// 1/leverage adverse move. This likely underestimates the true buffer OKX will report (which
	// includes maintenance margin), so it's a conservative floor for the hard-limit check, not an
	// exact liquidation price calculation.
	liqBufferPct := 0.0
	if targetLeverage > 0 {
		liqBufferPct = 100 / targetLeverage
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

	if approved.Leverage != currentLeverage && approved.Leverage > 0 {
		req := okx.SetLeverageRequest{InstID: t.InstID, Lever: fmt.Sprintf("%.2f", approved.Leverage), MgnMode: t.TdMode}
		if t.PosMode == "long_short" {
			req.PosSide = posSideFor(approved.PositionNotionalUSD)
		}
		if err := t.RESTClient.SetLeverage(req); err != nil {
			return fmt.Errorf("set leverage: %w", err)
		}
		logger.Info("leverage updated", "instId", t.InstID, "leverage", approved.Leverage)
	}

	// Preserve the sign of the (clamped) target notional so a negative TargetExposure still
	// produces a short target after risk clamping.
	signedTarget := approved.PositionNotionalUSD
	if targetNotional < 0 {
		signedTarget = -approved.PositionNotionalUSD
	}

	deltaNotional := signedTarget - currentNotional
	if mid <= 0 || (t.MinOrderUSD > 0 && absFloat(deltaNotional) < t.MinOrderUSD) {
		return nil
	}

	side := "buy"
	if deltaNotional < 0 {
		side = "sell"
	}
	// NOTE: sz is computed as underlying USD notional / mid price, i.e. it assumes a contract
	// multiplier of 1. Per-instrument contract-value handling (via /api/v5/public/instruments)
	// is not yet wired in — revisit before trading instruments with a non-1x contract value.
	sz := absFloat(deltaNotional) / mid

	order := okx.OrderRequest{
		InstID:  t.InstID,
		TdMode:  t.TdMode,
		Side:    side,
		OrdType: "market",
		Sz:      fmt.Sprintf("%.8f", sz),
	}
	if t.PosMode == "long_short" {
		order.PosSide = posSideFor(signedTarget)
	}

	result, err := t.RESTClient.PlaceOrder(order)
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
func posSideFor(signedNotional float64) string {
	if signedNotional < 0 {
		return "short"
	}
	return "long"
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
