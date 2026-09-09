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

	// ExecInstID/ExecInstType/SettleCcy mirror RealTrader's own fields (CLAUDE.md §27, 2026-09-04
	// design): InstID is now a short internal symbol ("BTC"), never OKX's own wire-format instId,
	// so every exchange call needs the real instId/instType/currency separately. Empty falls back
	// to InstID directly / instType "SWAP" / currency "USDT" — the pre-2026-09-04 behavior, for a
	// deployment whose account trades the classic SWAP product directly.
	ExecInstID   string
	ExecInstType string
	SettleCcy    string

	// ActiveTokens is the ordered roster the observation's token-identity one-hot is built against
	// (CLAUDE.md §15.1/§15.3) — it must be the same roster, in the same order, that the loaded
	// global model was trained with, since order defines each slot's index. Leaving it empty sends
	// an all-zero one-hot, which is a token the model has never seen.
	ActiveTokens []string

	// Mode is which account this trader operates against: "demo" or "real" (CLAUDE.md §15.6).
	// It selects which account_equity row the equity timeline is recorded under, and — critically —
	// "real" is the mode the repository refuses to auto-reset when drained (§15.7).
	Mode string
	// AccountInitialUSD is the configured starting balance, reported to the model alongside live
	// equity so it can see drawdown from the starting point the same way the paper path does.
	AccountInitialUSD decimal.Decimal
	// Repo records the equity timeline for this mode (CLAUDE.md §15.7's requirement that a drain be
	// visible after the fact, in every mode). Optional: nil means no timeline is recorded and the
	// observation reports the exchange's equity directly — the trading loop itself never depends on
	// it, so a database outage can't stop live trading.
	Repo port.Repository
}

// execInstID is the instId actually sent to the exchange — ExecInstID when set, else InstID.
func (t *Trader) execInstID() string {
	if t.ExecInstID != "" {
		return t.ExecInstID
	}
	return t.InstID
}

func (t *Trader) execInstType() string {
	if t.ExecInstType != "" {
		return t.ExecInstType
	}
	return "SWAP"
}

func (t *Trader) settleCcy() string {
	if t.SettleCcy != "" {
		return t.SettleCcy
	}
	return "USDT"
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
	mkt, err := t.Exchange.GetTicker(t.execInstID())
	if err != nil {
		return fmt.Errorf("fetch ticker: %w", err)
	}
	mid := mkt.Last

	positions, err := t.Exchange.GetPositions(t.execInstType())
	if err != nil {
		return fmt.Errorf("fetch positions: %w", err)
	}
	balances, err := t.Exchange.GetBalance(t.settleCcy())
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
		if p.InstID == t.execInstID() {
			pos = p
			break
		}
	}

	// CLAUDE.md §27.2: verify OKX actually applied the configured margin mode, rather than sending
	// TdMode on every request and never checking what came back — a config value that silently
	// didn't take effect (a stale position from before a config change, an OKX-side default, a
	// typo) must halt trading, not be traded through unnoticed. Only meaningful once a position
	// actually exists — pos.MgnMode on a flat/empty position response carries no information to
	// check against. Uses the SAME halt mechanism as the drawdown circuit breaker (risk.Manager.
	// Halt), not a separate flag, so the trading loop only ever needs to ask "am I halted."
	if !pos.Pos.IsZero() && pos.MgnMode != "" && pos.MgnMode != t.TdMode {
		t.RiskManager.Halt(fmt.Sprintf(
			"margin mode mismatch for %s: configured %q but OKX reports %q on the open position",
			t.InstID, t.TdMode, pos.MgnMode))
		logger.Error("margin mode mismatch, halting trading", "instId", t.InstID,
			"configured", t.TdMode, "reported", pos.MgnMode)
		return nil
	}

	posSize := pos.Pos
	lever := pos.Lever
	// OKX's own uplRatio is already leverage-adjusted (unrealized PnL / initial margin), matching
	// usecase.unrealizedPnLPct's convention on the paper-mode side (rl_sltp_adjust.go) since
	// 2026-08-31 — before that fix the two paths fed the model this field on different scales
	// (levered live, unlevered paper), so nothing here needs to change, only paper mode did.
	uplRatio := pos.UplRatio

	// The exchange's own reported equity is ground truth for demo/real — unlike paper mode, where
	// the balance is bookkeeping the engine owns, here the account really exists. It's reported as
	// AccountEquityUSD so the observation is shaped exactly like every paper-mode observation the
	// model trained on (CLAUDE.md §15.6).
	initial := t.AccountInitialUSD
	if !initial.IsPositive() {
		initial = equity
	}

	// The risk budget as an observation input (schema v7). This loop's limits are expressed as a
	// notional ceiling rather than a percentage, so convert against equity to give the model the
	// same fraction-of-account meaning every other caller sends.
	riskLimits := t.RiskManager.Limits()
	maxPositionPct := decimal.Zero
	if equity.IsPositive() {
		maxPositionPct = riskLimits.MaxPositionNotionalUSD.Div(equity)
	}

	obs := domain.Observation{
		SchemaVersion:     domain.ObservationSchemaVersion,
		InstID:            t.InstID,
		ActiveTokens:      t.ActiveTokens,
		LastPrice:         mid,
		Position:          posSize,
		CurrentLeverage:   lever,
		UnrealizedPnLPct:  uplRatio,
		AccountEquityUSD:  equity,
		AccountInitialUSD: initial,
		MaxPositionPct:    maxPositionPct,
		MaxLeverage:       riskLimits.MaxLeverage,
	}

	t.recordEquity(ctx, equity, initial, logger)

	action, err := t.Model.Predict(ctx, obs)
	if err != nil {
		return fmt.Errorf("rl predict: %w", err)
	}

	logger.Info("rl action received", "instId", t.InstID, "action", action.Action,
		"sizePct", action.SizePct, "leverageFrac", action.LeverageFrac, "confidence", action.Confidence)

	return t.execute(logger, mid, pos, posSize, lever, equity, action)
}

// recordEquity keeps this mode's equity timeline in step with what the exchange reports, so the
// panel can chart demo/real balance over time the same way it charts paper (CLAUDE.md §15.7's
// requirement that a drain be reviewable after the fact, in every mode).
//
// Unlike paper mode, the balance here isn't ours to compute — the exchange owns it. So rather than
// applying a PnL delta, this observes the reported equity and records the difference from what was
// last stored. A reset is never triggered from this path: for "real" the repository refuses to
// auto-reset by design, and for "demo" the exchange's own balance is authoritative, so topping up a
// local row would just desynchronize it from reality.
//
// Entirely best-effort: every failure is logged and swallowed, because a database problem must
// never interrupt a live trading loop.
func (t *Trader) recordEquity(ctx context.Context, equity, initial decimal.Decimal, logger *slog.Logger) {
	if t.Repo == nil || t.Mode == "" {
		return
	}

	acct, err := t.Repo.GetAccountEquity(ctx, t.Mode, initial)
	if err != nil {
		logger.Warn("equity timeline: read failed", "mode", t.Mode, "error", err)
		return
	}

	delta := equity.Sub(acct.EquityUSD)
	if delta.IsZero() {
		return // nothing moved since the last poll; don't spam the timeline with flat points
	}

	if _, _, err := t.Repo.ApplyRealizedPnL(ctx, t.Mode, delta, nil, t.InstID); err != nil {
		logger.Warn("equity timeline: write failed", "mode", t.Mode, "error", err)
	}
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

	// SizePct is UNSIGNED (CLAUDE.md §15.11) because in the paper path direction belongs to the
	// strategy that produced the signal. This loop has no strategy feeding it — it polls the model
	// directly (§14's open live-wiring item) — so it takes the side from Action.Side instead.
	// Anything other than an open means flat: skip/none/close all resolve to no exposure here.
	targetNotional := decimal.Zero
	if action.Action == domain.ActionOpen {
		targetNotional = action.SizePct.Mul(limits.MaxPositionNotionalUSD)
		if action.Side == "sell" {
			targetNotional = targetNotional.Neg()
		}
	}

	// Rough, conservative estimate of distance-to-liquidation as a percentage of mark price:
	// ignoring maintenance margin and fees, isolated-margin liquidation occurs at roughly a
	// 1/leverage adverse move. This likely underestimates the true buffer OKX will report (which
	// includes maintenance margin), so it's a conservative floor for the hard-limit check, not an
	// exact liquidation price calculation.
	liqBufferPct := decimal.Zero
	if targetLeverage.IsPositive() {
		liqBufferPct = decimal.NewFromInt(100).Div(targetLeverage)
	}

	// CLAUDE.md §27.2/§27.7: cross-check the rough estimate above against OKX's OWN reported
	// LiqPx/MarkPx for the position as it stands BEFORE this action — already fetched via
	// GetPositions every step, previously never consulted here. Only meaningful for an existing
	// position (a brand-new position has no LiqPx yet; OKX only populates it once one is open), and
	// deliberately only ever TIGHTENS the buffer used for Approve, never loosens it — a real
	// exchange-reported distance narrower than the rough estimate means the estimate is currently
	// wrong in the unsafe direction (underestimating risk, not overestimating it), which is exactly
	// the case this check exists to catch. The reverse (real buffer wider than the estimate) is
	// left alone: trusting the more conservative number in either direction, same "never let a
	// cross-check loosen a safety margin" pattern as the SL-cap layering (§19.2/§19.3).
	if !posSize.IsZero() && pos.MarkPx.IsPositive() && pos.LiqPx.IsPositive() {
		realBufferPct := pos.MarkPx.Sub(pos.LiqPx).Abs().Div(pos.MarkPx).Mul(decimal.NewFromInt(100))
		if realBufferPct.LessThan(liqBufferPct) {
			logger.Warn("liquidation buffer estimate was optimistic; using OKX's own reported distance instead",
				"instId", t.InstID, "estimatedPct", liqBufferPct.StringFixed(2), "realPct", realBufferPct.StringFixed(2),
				"markPx", pos.MarkPx, "liqPx", pos.LiqPx)
			liqBufferPct = realBufferPct
		}
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
		req := domain.LeverageChange{InstID: t.execInstID(), Lever: approved.Leverage, MgnMode: t.TdMode}
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
	// Converts through the instrument's own CtVal/LotSz (CLAUDE.md §27, found live 2026-09-04:
	// a raw notional/price division assumes a contract multiplier of 1, which sized an order
	// roughly 10,000x too large against this account's real X-Perp instrument). Reuses
	// sizeToContracts/instrumentMeta-shaped logic via a direct GetInstrument call rather than
	// caching per-call the way RealTrader does — this path polls once every PollInterval, not
	// once per order, so the extra call is not the hot path RealTrader's sync.Once optimizes for.
	inst, err := t.Exchange.GetInstrument(t.execInstType(), t.execInstID())
	if err != nil {
		return fmt.Errorf("fetch instrument metadata: %w", err)
	}
	// Leverage 1 deliberately: deltaNotional is ALREADY a position notional here (this path
	// computes a target exposure directly), unlike RealTrader's own call site which passes margin
	// and needs leverage applied to reach the notional. Passing the real leverage would multiply a
	// notional that already accounts for it.
	sz := sizeToContracts(deltaNotional.Abs(), decimal.NewFromInt(1), mid, inst)
	if sz.IsZero() || (inst.MinSz.IsPositive() && sz.LessThan(inst.MinSz)) {
		return nil
	}

	order := domain.OrderRequest{
		InstID:  t.execInstID(),
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
