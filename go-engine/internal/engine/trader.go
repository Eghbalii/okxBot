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

	// Translating action -> concrete orders (sizing, side, and order submission) is
	// intentionally left as the next implementation step once the RL service and a backtest
	// harness exist to validate behavior before it touches real order placement.
	return nil
}
