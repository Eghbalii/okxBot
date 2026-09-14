package backtest

import (
	"errors"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// Reasons a signal produces no sample. Named errors rather than free strings so the Skipped
// breakdown in Result has a fixed vocabulary — a run that discards most of its signals must be
// visibly different from a quiet market, and a typo'd label would hide that.
var (
	errNoStop        = errors.New("no_stop")
	errClampRejected = errors.New("clamp_rejected")
	errObsInvalid    = errors.New("observation_invalid")
	errNoEquity      = errors.New("no_equity")
)

// openPosition turns a signal into a simulated trade and captures the observation the decision was
// made from.
//
// The observation is built by the SAME usecase builders the live engine uses. That is the load-
// bearing property of this whole package: a separate builder here would be a second definition of
// the model's input, free to disagree with the one being served — and a warm start trained on a
// different input shape than production sends is worse than no warm start at all.
func (r *Runner) openPosition(
	instID, bar, kind string,
	sig strategy.Signal,
	window, btcWindow []domain.Candle,
	idx int,
	c domain.Candle,
) (*position, error) {
	if !r.account.IsPositive() {
		// A drained simulated account. The live engine resets it (§15.7); here the run simply stops
		// producing samples for this instrument, because a reset mid-dataset would teach the policy
		// that losses are wiped clean — the same lesson §15.6 removed from the old per-token accounts.
		return nil, errNoEquity
	}

	price := c.Close
	resolved := sig.ResolveLevels(price)

	// Size exactly as the live fixed-sizing path does: equity split evenly across slots (§32.4).
	slots := r.Cfg.PositionSlots
	if slots <= 0 {
		slots = 1
	}
	size := r.account.Div(decimal.NewFromInt(int64(slots))).Round(8)
	leverage := r.Cfg.MaxLeverage
	if !leverage.IsPositive() {
		leverage = decimal.NewFromInt(1)
	}

	side := "buy"
	if sig.Side == strategy.Sell {
		side = "sell"
	}

	// The same clamps production applies at open (§19.2's 15% loss cap, §45's reward:risk bound).
	// Training without them would let the policy learn placements Go silently rejects — and, worse,
	// score them as though they had been taken.
	in := conductor.Levels{}
	if resolved.SLPx.IsPositive() {
		v := resolved.SLPx
		in.SLPx = &v
	}
	if resolved.TPPx.IsPositive() {
		v := resolved.TPPx
		in.TPPx = &v
	}
	clamps := r.Cfg.Clamps
	lv := clamps.Apply(side, price, leverage, clamps.EnsureStop(side, price, leverage, in))
	if lv.SLPx == nil || !lv.SLPx.IsPositive() {
		// §16.9: an order with no stop is unbounded downside, refused outright rather than opened.
		return nil, errNoStop
	}

	obs, err := r.buildObservation(instID, bar, kind, price, window, btcWindow, sig, resolved, lv)
	if err != nil {
		return nil, errObsInvalid
	}
	obs.Category = domain.CategoryBuy
	if side == "sell" {
		obs.Category = domain.CategorySell
	}
	r.nextOrderID++
	obs.OrderID = r.nextOrderID

	return &position{
		instID: instID, bar: bar, kind: kind, side: side,
		entryPx: price, slPx: lv.SLPx, tpPx: lv.TPPx,
		size: size, leverage: leverage,
		openedAt: c.Timestamp, openedIdx: idx,
		openObs: obs,
	}, nil
}

// closePosition realizes a trade, scores it, and emits the sample.
func (r *Runner) closePosition(
	p *position,
	exit decimal.Decimal,
	reason string,
	c domain.Candle,
	idx int,
	btcWindow []domain.Candle,
) {
	pnl, fees := realizedPnL(p, exit)

	// The shared account moves before the reward is computed, so the drawdown term sees the state
	// this trade actually left behind — matching the live path, where equity is written on close
	// and the next observation reads it.
	r.account = r.account.Add(pnl).Round(8)
	if r.account.GreaterThan(r.peak) {
		r.peak = r.account
	}

	reward := TradeReward(RewardInput{
		RealizedPnLUSD: pnl,
		FeesUSD:        fees,
		RiskPct:        riskPct(p),
		PositionSize:   p.size,
		Leverage:       p.leverage,
		EquityUSD:      r.account,
		PeakEquityUSD:  r.peak,
		Adjustments:    p.adjustments,
	})

	// The terminal observation, shaped as the live reward-delivery call (§15.10). Entry/SL/TP are
	// deliberately NOT zeroed: the outcome has to stay attached to the decision that produced it.
	term := p.openObs
	term.Category = terminalCategory(reason)
	// OrderID carries over unchanged from the opening observation: it is what pairs this outcome
	// with the decision that produced it.
	term.LastPrice = exit
	term.PositionState = domain.PositionState{
		PositionOpen:     true,
		Side:             sideSign(p.side),
		SizeUSD:          p.size,
		Leverage:         p.leverage,
		AgeSeconds:       int64(c.Timestamp.Sub(p.openedAt).Seconds()),
		UnrealizedPnLPct: pnlPct(p, exit),
		PnLMaxPct:        p.pnlMaxPct,
		PnLMinPct:        p.pnlMinPct,
		RealizedPnLUSD:   pnl,
		RiskPct:          riskPct(p),
		SLTPAdjustments:  p.adjustments,
	}

	rec := r.records[p.kind]
	if rec == nil {
		rec = &stratRecord{}
		r.records[p.kind] = rec
	}
	rec.trades++
	if pnl.IsPositive() {
		rec.wins++
		r.result.Wins++
	} else {
		r.result.Losses++
	}
	rec.pnlSum = rec.pnlSum.Add(pnl.Div(maxDec(p.size, decimal.NewFromInt(1))))
	rec.holdSum += c.Timestamp.Sub(p.openedAt)
	if rr := rewardRisk(p); rr.IsPositive() {
		rec.rrSum = rec.rrSum.Add(rr)
	}

	pnlF, _ := pnl.Float64()
	entryF, _ := p.entryPx.Float64()
	exitF, _ := exit.Float64()

	r.result.Samples++
	r.result.ByReason[reason]++
	r.result.TotalPnL += pnlF
	r.result.MeanReward += reward

	if err := r.Sink.Write(Sample{
		Observation: p.openObs,
		Terminal:    term,
		Reward:      reward,
		RealizedPnL: pnlF,
		CloseReason: reason,
		InstID:      p.instID,
		Bar:         p.bar,
		Kind:        p.kind,
		Side:        p.side,
		OpenedAt:    p.openedAt,
		ClosedAt:    c.Timestamp,
		EntryPx:     entryF,
		ExitPx:      exitF,
		HoldBars:    idx - p.openedIdx,
		Adjustments: p.adjustments,
	}); err != nil {
		r.Logger.Warn("backtest: sink write failed", "error", err)
	}
}

// buildObservation assembles the model input, from the same builders the live engine uses.
func (r *Runner) buildObservation(
	instID, bar, kind string,
	price decimal.Decimal,
	window, btcWindow []domain.Candle,
	sig strategy.Signal,
	resolved strategy.Signal,
	lv conductor.Levels,
) (domain.Observation, error) {
	mb, err := usecase.BuildMarketBlock(bar, window)
	if err != nil {
		return domain.Observation{}, err
	}
	btc, err := usecase.BuildBTCContext(btcWindow, mb.ClosePctChanges)
	if err != nil {
		return domain.Observation{}, err
	}

	// The strategy's profile is what THIS RUN has recorded so far — not the production database's
	// figures. That is what makes a forward simulation coherent where rebuilding historical rows is
	// not: the simulation always knows its own books, and starts from zero like a fresh install.
	rec := r.records[kind]
	if rec == nil {
		rec = &stratRecord{}
	}
	profile := rec.profile(kind, bar)
	profile.Side = string(sig.Side)
	profile.EntryPx = resolved.EntryPx
	if lv.SLPx != nil {
		profile.SLPx = *lv.SLPx
	}
	if lv.TPPx != nil {
		profile.TPPx = *lv.TPPx
	}

	slots := r.Cfg.PositionSlots
	if slots <= 0 {
		slots = 1
	}

	obs := domain.Observation{
		SchemaVersion:     domain.ObservationSchemaVersion,
		InstID:            instID,
		LastPrice:         price,
		TokenProfile:      tokenProfile(window),
		Timeframes:        []domain.MarketBlock{mb},
		BTC:               btc,
		Signal:            &profile,
		AccountEquityUSD:  r.account,
		AccountInitialUSD: r.Cfg.InitialUSD,
		AccountPeakUSD:    r.peak,
		MaxPositionPct:    decimal.NewFromInt(1).Div(decimal.NewFromInt(int64(slots))),
		MaxLeverage:       r.Cfg.MaxLeverage,
		Category:          domain.CategoryUpdate,
	}
	if err := obs.Validate(); err != nil {
		return domain.Observation{}, err
	}
	return obs, nil
}

// tokenProfile describes the instrument from its own candles.
//
// Only the candle-derived half: volatility and price magnitude. The roster-wide figures (volume,
// rank, 24h change) come from the discovery scan's live snapshot, which has no historical record —
// so they are left zero here rather than backfilled with today's values, which would be lookahead
// on a field the policy reads as current market state.
func tokenProfile(window []domain.Candle) domain.TokenProfile {
	p := domain.TokenProfile{}
	if len(window) == 0 {
		return p
	}
	last := window[len(window)-1]
	if !last.Close.IsPositive() {
		return p
	}
	if f, _ := last.Close.Float64(); f > 0 {
		p.LogPrice = decimal.NewFromFloat(log10(f))
	}
	if atr, err := strategy.ATR(window, 14); err == nil {
		p.TypicalVolatility = atr.Div(last.Close)
	}
	return p
}

func terminalCategory(reason string) string {
	switch reason {
	case "sl":
		return domain.CategoryClosedSL
	case "tp":
		return domain.CategoryClosedTP
	case "timeout":
		return domain.CategoryClosedTimeout
	default:
		return domain.CategoryClosedEarly
	}
}

func sideSign(side string) decimal.Decimal {
	if side == "sell" {
		return decimal.NewFromInt(-1)
	}
	return decimal.NewFromInt(1)
}

// rewardRisk is the trade's reward:risk as placed, for the strategy profile's AvgRR.
func rewardRisk(p *position) decimal.Decimal {
	if p.slPx == nil || p.tpPx == nil {
		return decimal.Zero
	}
	risk := p.entryPx.Sub(*p.slPx).Abs()
	if !risk.IsPositive() {
		return decimal.Zero
	}
	return p.tpPx.Sub(p.entryPx).Abs().Div(risk)
}

func maxDec(a, b decimal.Decimal) decimal.Decimal {
	if a.GreaterThan(b) {
		return a
	}
	return b
}
