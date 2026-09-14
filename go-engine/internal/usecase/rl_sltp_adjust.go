package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// swingWindow is how many recent candles back the distance-to-swing-high/low features look
// (CLAUDE.md §15.3) — short enough to reflect the current local structure, not the whole window.
const swingWindow = 20

// priceContextWindow is how many recent bar-over-bar returns are sent as PriceContext.ClosePctChanges.
const priceContextWindow = 10

// recentTradesWindow bounds how many of this token's most-recent closed baseline trades are sent
// as the observation's recent-performance tail (CLAUDE.md §15.3/§15.7).
const recentTradesWindow = 10

// rlSizing asks the RL agent how much of the SHARED account to put behind a strategy signal that
// has already decided to trade (CLAUDE.md §15.6/§15.11): SizePct is a fraction of current account
// equity, LeverageFrac maps onto [1x, MaxLeverage]. Returns ok=false — leaving the caller's
// fixed sizing untouched — when RL sizing is disabled, unconfigured, the model errors, or the model
// asks for effectively no exposure.
//
// Two hard Go-side caps bound the result regardless of what the model proposes, the same
// "never trust the model as the safety boundary" pattern as §15.4's SL/TP ratchet and §5's risk
// manager:
//
//   - MaxPositionPct — no single position may exceed this fraction of equity, so one confident
//     early decision can't put most of the account into one trade.
//   - MaxTotalExposurePct — the sum of all currently-open positions' notionals is capped too,
//     since the per-position cap alone still permits N simultaneous positions at the cap.
//
// Deliberately magnitude-only: the strategy layer owns direction (CLAUDE.md §9/§16.1 — strategies
// decide *when* there's a tradeable signal, the agent decides *how much*), so a model that
// disagrees with the signal's side expresses that by sizing down toward zero, never by flipping
// the order's side out from under the strategy that generated it.
func (e *PaperTrader) rlSizing(
	ctx context.Context,
	obs domain.Observation,
	signal strategy.Signal,
	openOrders []port.PaperOrder,
	logger *slog.Logger,
) (notional, leverage decimal.Decimal, ok bool) {
	if !e.RLSizing || e.Model == nil || !e.MaxLeverage.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}

	equity := obs.AccountEquityUSD
	if !equity.IsPositive() {
		// A drained (or unreadable) account has nothing to size against. Falling back to the fixed
		// notional here would quietly ignore the drain, so decline instead and let the caller's
		// own budget check decide whether to trade at all.
		logger.Warn("rl sizing: no positive account equity to size against, falling back",
			"instId", e.InstID, "equity", equity)
		return decimal.Zero, decimal.Zero, false
	}

	action, err := e.Model.Predict(ctx, obs)
	if err != nil {
		logger.Warn("rl sizing: predict failed, falling back to fixed sizing",
			"instId", e.InstID, "error", err)
		return decimal.Zero, decimal.Zero, false
	}

	// A `skip` is the model declining this signal outright (CLAUDE.md §15.11) — a decision it can
	// now express, where before sizing to zero was indistinguishable from "no opinion".
	if action.Action == domain.ActionSkip {
		logger.Info("rl sizing: model declined the signal", "instId", e.InstID, "side", signal.Side)
		return decimal.Zero, decimal.Zero, false
	}

	return e.sizeFromAction(action, obs, openOrders, logger)
}

// sizingConfig is the sizing-relevant slice of PaperTrader's config, extracted so
// sizeFromModelAction can be shared verbatim by RealTrader (CLAUDE.md §27's plan, commit 2) without
// either type needing to embed the other. Position sizing math is the one piece of real financial
// logic that MUST be identical between paper and real trading, so sharing the literal function is
// the safest option, not merely the least-duplicate one.
type sizingConfig struct {
	InstID              string
	MaxLeverage         decimal.Decimal
	MaxPositionPct      decimal.Decimal
	MaxTotalExposurePct decimal.Decimal
}

// sizeFromAction adapts PaperTrader's fields to sizeFromModelAction. Kept as a thin method so every
// existing call site inside this package is unaffected by the extraction.
func (e *PaperTrader) sizeFromAction(
	action *domain.Action,
	obs domain.Observation,
	openOrders []port.PaperOrder,
	logger *slog.Logger,
) (notional, leverage decimal.Decimal, ok bool) {
	cfg := sizingConfig{
		InstID:              e.InstID,
		MaxLeverage:         e.MaxLeverage,
		MaxPositionPct:      e.MaxPositionPct,
		MaxTotalExposurePct: e.MaxTotalExposurePct,
	}
	return sizeFromModelAction(cfg, action, obs, openOrders, logger)
}

// sizeFromModelAction turns a model action into a position notional and leverage, applying the two
// hard Go-side caps documented on rlSizing. Split out from rlSizing so the lifecycle's open decision
// (which has already called the model to get the buy/sell answer) can reuse the exact same sizing
// and capping rules without issuing a second Predict for one decision — two calls would not only
// waste inference, they could return different answers and leave the order sized against one while
// its levels came from the other. A free function (not a PaperTrader method) so RealTrader can call
// it directly against its own sizingConfig, with zero risk of drift between the two.
func sizeFromModelAction(
	cfg sizingConfig,
	action *domain.Action,
	obs domain.Observation,
	openOrders []port.PaperOrder,
	logger *slog.Logger,
) (notional, leverage decimal.Decimal, ok bool) {
	equity := obs.AccountEquityUSD
	if !equity.IsPositive() || !cfg.MaxLeverage.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}

	exposure := clampUnit(action.SizePct.Abs())
	if !exposure.IsPositive() {
		// No exposure requested but no explicit skip either; keep the fixed sizing rather than
		// opening a meaningless zero-size order.
		return decimal.Zero, decimal.Zero, false
	}

	notional = equity.Mul(exposure)

	if cfg.MaxPositionPct.IsPositive() {
		if cap := equity.Mul(cfg.MaxPositionPct); notional.GreaterThan(cap) {
			logger.Info("rl sizing: position capped", "instId", cfg.InstID,
				"requested", notional, "cap", cap, "maxPositionPct", cfg.MaxPositionPct)
			notional = cap
		}
	}

	if cfg.MaxTotalExposurePct.IsPositive() {
		var openNotional decimal.Decimal
		for _, o := range openOrders {
			// Forks are tracking-only shadows of their baseline parent (CLAUDE.md §15.4), not
			// separate capital at risk — counting them would double-charge one signal's exposure.
			if o.Variant == "baseline" || o.Variant == "" {
				openNotional = openNotional.Add(o.Size)
			}
		}
		headroom := equity.Mul(cfg.MaxTotalExposurePct).Sub(openNotional)
		if headroom.LessThanOrEqual(decimal.Zero) {
			logger.Info("rl sizing: total exposure ceiling reached, falling back",
				"instId", cfg.InstID, "openNotional", openNotional,
				"maxTotalExposurePct", cfg.MaxTotalExposurePct)
			return decimal.Zero, decimal.Zero, false
		}
		if notional.GreaterThan(headroom) {
			logger.Info("rl sizing: position trimmed to remaining exposure headroom",
				"instId", cfg.InstID, "requested", notional, "headroom", headroom)
			notional = headroom
		}
	}

	if !notional.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}

	leverage = decimal.NewFromInt(1).Add(clampUnit(action.LeverageFrac).Mul(cfg.MaxLeverage.Sub(decimal.NewFromInt(1))))

	logger.Debug("rl sizing applied", "instId", cfg.InstID,
		"equity", equity, "exposure", exposure, "notional", notional, "leverage", leverage)
	return notional, leverage, true
}

// clampUnit clamps d into [0, 1] — the range every fractional model output is defined over.
func clampUnit(d decimal.Decimal) decimal.Decimal {
	one := decimal.NewFromInt(1)
	switch {
	case d.IsNegative():
		return decimal.Zero
	case d.GreaterThan(one):
		return one
	default:
		return d
	}
}

// buildObservation assembles the v8 observation for this token (docs/RL_V8_PLAN.md).
//
// Returns an ERROR rather than a partly-filled observation, which is the central discipline of v8.
// Before this it returned a struct unconditionally: a failed GetAccountEquity left the balance at
// zero (which makes three separate ratios read zero, telling the model the account is empty) and a
// short candle window yielded fewer returns than the vector expects, absorbed downstream by
// padding. Both looked healthy from every log. A caller that gets an error here must SKIP the model
// call — not send it and let rl_service reject it, because a rejected call leaves a pending
// decision in the learner that never receives its reward (§15.12's gap, reopened).
//
// The candle window/market block reflect the most recent bar; LastPrice and the per-order PnL
// fields reflect whatever price the caller passes — the live tick from handleTick (§15.9), a candle
// close from evaluateStrategies' open path (still candle-driven by design, §9).
func (e *PaperTrader) buildObservation(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) (domain.Observation, error) {
	view := e.marketView(bar)
	window := view.Candles

	mb, err := BuildMarketBlock(bar, window)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("market block: %w", err)
	}

	btc, err := e.btcContext(bar, mb.ClosePctChanges)
	if err != nil {
		return domain.Observation{}, err
	}

	acct, err := e.Repo.GetAccountEquity(ctx, e.accountMode(), e.AccountInitialUSD)
	if err != nil {
		// No longer a Warn-and-continue. Equity feeds three ratios plus the position-size budget,
		// so a zero here is not one missing number but a coherent-looking lie about the account.
		return domain.Observation{}, fmt.Errorf("account equity: %w", err)
	}

	exposure, levExposure, openCount, err := e.exposureSnapshot(ctx)
	if err != nil {
		return domain.Observation{}, fmt.Errorf("open exposure: %w", err)
	}

	obs := domain.Observation{
		SchemaVersion:            domain.ObservationSchemaVersion,
		InstID:                   e.InstID,
		LastPrice:                price,
		TokenProfile:             e.tokenProfile(window),
		Timeframes:               []domain.MarketBlock{mb},
		BTC:                      btc,
		AccountEquityUSD:         acct.EquityUSD,
		AccountInitialUSD:        e.AccountInitialUSD,
		AccountPeakUSD:           e.peakEquity(acct.EquityUSD),
		OpenExposureUSD:          exposure,
		OpenLeveragedExposureUSD: levExposure,
		OpenPositionCount:        openCount,
		// MaxPositionPct is the per-token EVEN SHARE of the account (1/PositionSlots), not
		// account.max_position_pct: the fixed sizing path (dynamicNotional) divides equity evenly
		// across slots, so that share is what one position is actually expected to take. Deriving
		// it from the live slot count means adding or disabling a token reshapes the budget
		// automatically — the whole reason the caps are an input rather than baked into output
		// scaling. account.max_position_pct still applies afterwards as the hard ceiling.
		MaxPositionPct: e.evenShareOfAccount(),
		MaxLeverage:    e.MaxLeverage,
		// Category and Signal are always set by the caller, the only place that knows which decision
		// is being asked (§15.12). CategoryUpdate stands as a fallback because it is the one
		// category that claims nothing — no strategy spoke, no outcome is being reported — so a
		// caller that forgot to set one cannot accidentally train the model on a reward or an entry
		// decision that never happened.
		Category: domain.CategoryUpdate,
	}
	return obs, nil
}

// btcContext builds the market-wide reference block from BTC's own candle window on the same bar.
//
// A token with no BTC window available is an error rather than a zeroed block: zeros would read as
// "BTC is perfectly flat and uncorrelated", which is a specific and false claim about the market,
// not an absence of information.
func (e *PaperTrader) btcContext(bar string, tokenReturns []decimal.Decimal) (domain.BTCContext, error) {
	if e.BTCCandles == nil {
		return domain.BTCContext{}, fmt.Errorf("btc context: no reference feed wired")
	}
	window, ok := e.BTCCandles(bar)
	if !ok || len(window) == 0 {
		return domain.BTCContext{}, fmt.Errorf("btc context: no %s window yet", bar)
	}
	return BuildBTCContext(window, tokenReturns)
}

// tokenProfile describes what this instrument IS, replacing v7's identity one-hot.
//
// Volatility and price come from the candle window this call already holds; volume, rank and the
// 24h figures come from the discovery scan's own market snapshot (§53) when one is wired, since
// those are roster-wide facts a single instrument's engine has no other way to know.
func (e *PaperTrader) tokenProfile(window []domain.Candle) domain.TokenProfile {
	p := domain.TokenProfile{}
	if len(window) == 0 {
		return p
	}
	last := window[len(window)-1]
	if last.Close.IsPositive() {
		f, _ := last.Close.Float64()
		if f > 0 {
			p.LogPrice = decimal.NewFromFloat(math.Log10(f))
		}
		if atr, err := strategy.ATR(window, atrPeriod); err == nil {
			p.TypicalVolatility = atr.Div(last.Close)
		}
	}
	if e.TokenStats != nil {
		s := e.TokenStats(e.InstID)
		p.LogVolume24h = s.LogVolume24h
		p.VolumeRank = s.VolumeRank
		p.Range24h = s.Range24h
		p.Change24h = s.Change24h
		p.LogTradeCount = s.LogTradeCount
	}
	return p
}

// peakEquity is the high-water mark this process has observed, so drawdown is measured from the
// peak rather than from the configured starting balance. v7 fed equity/initial, and SetAccountCap
// rewrites initial (§32.2) — so every cap change wiped the model's view of drawdown back to ~1.0.
//
// Process-local: losing it on restart means the mark resets to the current balance, which
// understates drawdown until a new peak is set. That is the conservative direction (it cannot
// invent a drawdown that did not happen) and it costs nothing to correct itself.
func (e *PaperTrader) peakEquity(equity decimal.Decimal) decimal.Decimal {
	e.peakMu.Lock()
	defer e.peakMu.Unlock()
	if equity.GreaterThan(e.peak) {
		e.peak = equity
	}
	return e.peak
}

// levelAdjustPct expresses "move this level from current to proposed" as a fraction of the live
// price, which is the form RatchetSLTP takes. The model emits levels (CLAUDE.md §15.11) while the
// ratchet reasons in relative moves, so this is the seam between the two.
//
// Returns zero when either level is missing or the price is unusable, so a nil SL/TP simply means
// "nothing proposed" rather than a spurious move away from zero.
func levelAdjustPct(current *decimal.Decimal, proposed, price decimal.Decimal) decimal.Decimal {
	if current == nil || !proposed.IsPositive() || !price.IsPositive() {
		return decimal.Zero
	}
	return proposed.Sub(*current).Div(price)
}

// computeAdjustedLevels is the pure half of applyAdjustment: given an order, the model's proposed
// action, and the current price, it returns the ratchet-checked new SL/TP and whether anything
// actually changed. Extracted as a free function (CLAUDE.md §27's plan, commit 2) so RealTrader's
// in-place-edit update path (no shadow fork, §27.3) can reuse the exact same computation the
// already-proven paper-trading path uses, with the IO (persisting, logging, recording the audit
// row) left to each caller since paper and real trading write to different places.
func computeAdjustedLevels(o port.PaperOrder, action *domain.Action, price, minSLDistPct decimal.Decimal) (newSL, newTP *decimal.Decimal, changed bool) {
	// The model sets levels (§15.11) while the ratchet reasons in relative moves, so convert here.
	slAdjust := levelAdjustPct(o.SLPx, action.SLPx, price)
	tpAdjust := levelAdjustPct(o.TPPx, action.TPPx, price)
	if slAdjust.IsZero() && tpAdjust.IsZero() {
		return o.SLPx, o.TPPx, false
	}

	newSL, newTP = RatchetSLTP(o, price, minSLDistPct, slAdjust, tpAdjust)
	if samePriceOrNil(newSL, o.SLPx) && samePriceOrNil(newTP, o.TPPx) {
		return o.SLPx, o.TPPx, false // the ratchet rejected the proposal entirely; nothing to apply
	}
	return newSL, newTP, true
}

// positionStateOf describes an open order for the observation (CLAUDE.md §15.10). Prices are sent
// as-is; rl_service converts them to fractions of the live price at vectorization time, so one
// shared policy generalizes across instruments at wildly different price scales.
func positionStateOf(o port.PaperOrder, price decimal.Decimal) domain.PositionState {
	side := decimal.NewFromInt(1)
	if o.Side == "sell" {
		side = decimal.NewFromInt(-1)
	}
	ps := domain.PositionState{
		PositionOpen:     true,
		Side:             side,
		SizeUSD:          o.Size,
		Leverage:         o.Leverage,
		UnrealizedPnLPct: unrealizedPnLPct(o, price),
		PnLMaxPct:        o.PnLMaxPct,
		PnLMinPct:        o.PnLMinPct,
		DistToSLPct:      distPct(o.SLPx, price),
		DistToTPPct:      distPct(o.TPPx, price),
	}
	if !o.OpenedAt.IsZero() {
		ps.AgeSeconds = int64(time.Since(o.OpenedAt).Seconds())
	}
	return ps
}

// exposureSnapshot reports how much of the shared account is already committed across ALL tokens
// (CLAUDE.md §15.6): with one pool, the agent has to see what is spent elsewhere before asking for
// more.
//
// Returns BOTH margin committed and leveraged exposure. v7 sent only the first, which is notional
// before leverage — a $10 position at 10x counted as $10 of committed risk when it is really $100,
// understating real market exposure by exactly the leverage factor.
//
// No longer best-effort: a read failure used to report zero exposure, telling the model the account
// is entirely free when it may be fully committed.
func (e *PaperTrader) exposureSnapshot(ctx context.Context) (margin, leveraged decimal.Decimal, count int, err error) {
	openOnly := true
	positions, err := e.Repo.ListPositions(ctx, port.PositionFilter{Mode: e.accountMode(), Open: &openOnly})
	if err != nil {
		return decimal.Zero, decimal.Zero, 0, err
	}
	for _, p := range positions {
		margin = margin.Add(p.Size)
		lev := p.Leverage
		if !lev.IsPositive() {
			lev = decimal.NewFromInt(1)
		}
		leveraged = leveraged.Add(p.Size.Mul(lev))
		count++
	}
	return margin, leveraged, count, nil
}

// unrealizedPnLPct is the position's unrealized PnL as a fraction of margin (entry-to-price move
// scaled by leverage), matching OKX's own uplRatio convention (internal/okx's live/demo path,
// usecase/trade.go) and the dollar PnL math elsewhere in this package (realizedPnL,
// papertrade.go), which both already multiply by leverage. Before 2026-08-31 this returned the
// raw price-change ratio with no leverage applied at all — correct only by coincidence at the
// service's old 1x default, and silently wrong at any other leverage (a 1% price move at 20x
// showed as 1% instead of 20%). That mismatch also meant paper-mode observations (built from this
// function) and live/demo observations (built from OKX's already-levered uplRatio, trade.go) fed
// the model two different scales for the same field — fixed together, not just the display.
func unrealizedPnLPct(o port.PaperOrder, price decimal.Decimal) decimal.Decimal {
	if !o.EntryPx.IsPositive() {
		return decimal.Zero
	}
	direction := decimal.NewFromInt(1)
	if o.Side == "sell" {
		direction = decimal.NewFromInt(-1)
	}
	leverage := o.Leverage
	if !leverage.IsPositive() {
		leverage = decimal.NewFromInt(1)
	}
	return direction.Mul(price.Sub(o.EntryPx)).Div(o.EntryPx).Mul(leverage)
}

func distPct(px *decimal.Decimal, price decimal.Decimal) decimal.Decimal {
	if px == nil || !price.IsPositive() {
		return decimal.Zero
	}
	return px.Sub(price).Div(price)
}

func samePriceOrNil(a, b *decimal.Decimal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
