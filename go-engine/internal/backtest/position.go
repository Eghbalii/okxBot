package backtest

import (
	"errors"
	"math"

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
	// A drained account is RESET, exactly as paper trading resets it (§15.7): a losing streak early
	// in training is expected noise, not a reason to stop trading, and the live system acts on that
	// belief. The first version of this refused to reset, reasoning that it would teach the policy
	// losses are wiped clean — which measured badly on real data: the account reached $0.000007
	// after 5,663 trades and the remaining 21,000 samples were opened at sizes no real account
	// would ever take. Training on those would be the train/serve skew this whole rewrite exists to
	// remove, since production would have reset and carried on at a normal size.
	//
	// The reset is recorded in the run summary rather than hidden: an account draining repeatedly
	// is itself the finding, which is exactly why §15.7 counts resets instead of papering over them.
	if r.account.LessThan(minTradableUSD) {
		r.account = r.Cfg.InitialUSD
		r.peak = r.Cfg.InitialUSD
		r.result.Resets++
	}

	price := c.Close
	resolved := sig.ResolveLevels(price)

	// Size exactly as the live fixed-sizing path does: equity split evenly across slots (§32.4).
	slots := r.Cfg.PositionSlots
	if slots <= 0 {
		slots = 1
	}
	size := r.account.Div(decimal.NewFromInt(int64(slots))).Round(8)
	// The per-position cap production applies on top of the even split (§15.6's
	// account.max_position_pct). Without it a run configured with few slots lets one trade commit
	// the whole balance at full leverage — which compounds: measured on the test fixture with one
	// slot, a $2 account reached $124 million, an outcome no live configuration can produce because
	// this cap is what prevents it.
	if cap := r.Cfg.MaxPositionPct; cap.IsPositive() {
		if limit := r.account.Mul(cap).Round(8); size.GreaterThan(limit) {
			size = limit
		}
	}
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
		// The cadence baseline starts at the open, matching conductor.ShouldUpdate's first call
		// which records state and returns false. Left at the zero time, every position would fire
		// an update on its very next candle because "now minus zero" exceeds any ceiling.
		lastUpdateAt: c.Timestamp,
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

	ks := r.result.ByStrategy[p.kind]
	if ks == nil {
		ks = &KindStats{Kind: p.kind}
		r.result.ByStrategy[p.kind] = ks
	}
	ks.Trades++
	if pnl.IsPositive() {
		ks.Wins++
	}
	ks.PnLUSD += pnlF
	ks.rewardSum += reward
	ks.holdSum += idx - p.openedIdx

	r.pnls = append(r.pnls, pnlF)
	// Counted at CLOSE, not at open: the profile answers "how much has this token taught me", and
	// an open trade has taught nothing yet. Same rule the strategy profile already follows.
	r.tokenTrades[p.instID]++
	r.result.Samples++
	r.result.ByReason[reason]++
	r.result.TotalPnL += pnlF
	r.result.MeanReward += reward

	// Every buffered `update` resolves to the same outcome, because each of them answered the same
	// question — keep holding this position — and this is what holding produced. The terminal
	// observation is their next_obs for the same reason it is the open decision's.
	for _, u := range p.updates {
		if err := r.Sink.Write(Sample{
			Observation: u,
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
		r.result.Samples++
	}

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
		TokenProfile:      tokenProfile(window, barsPerDay(bar), r.volumeRank[instID], r.tokenTrades[instID]),
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
// Every field is computed from the candle window AS IT STOOD at the decision, never from a live
// snapshot. An earlier version left volume, rank, 24h range and 24h change at zero on the reasoning
// that they come from the discovery scan and have no historical record. That reasoning was right
// about the scan and wrong about the data: `candles.volume` is stored per bar, so the same window
// that yields ATR yields all four — measured over the trailing 24h rather than read from today.
//
// This mattered more than it looks. The profile replaced the token one-hot (docs/RL_V8_PLAN.md) so
// the policy could tell a BTC from a PEPE by what the token IS. With five of its seven inputs dead,
// it could see only price magnitude and volatility, and every other token distinction was invisible.
//
// barsPerDay scales the lookback to the timeframe (288 on 5m, 24 on 1H). rank is the token's
// position in the run's volume ordering, which the caller supplies because it is roster-wide.
func tokenProfile(window []domain.Candle, barsPerDay int, rank decimal.Decimal, tradeCount int) domain.TokenProfile {
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
	p.VolumeRank = rank
	// log1p, not log10: a token's first trade is 0 and log10(0) is -Inf, which would poison the
	// whole vector rather than reading as "nothing known here yet".
	p.LogTradeCount = decimal.NewFromFloat(math.Log1p(float64(tradeCount)))

	// The trailing day, or the whole window when history is shorter — a short window is the normal
	// warm-up case, not an error, and reporting zero there would be the very defect this fixes.
	if barsPerDay <= 0 {
		barsPerDay = 288
	}
	day := window
	if len(day) > barsPerDay {
		day = day[len(day)-barsPerDay:]
	}
	hi, lo, vol := day[0].High, day[0].Low, decimal.Zero
	for _, c := range day {
		if c.High.GreaterThan(hi) {
			hi = c.High
		}
		if c.Low.LessThan(lo) {
			lo = c.Low
		}
		// Quote volume: base volume times price. §33.4's own ranking lesson — a contract count
		// orders the market by contract size rather than by liquidity.
		vol = vol.Add(c.Volume.Mul(c.Close))
	}
	if lo.IsPositive() {
		p.Range24h = hi.Sub(lo).Div(lo)
	}
	if open := day[0].Open; open.IsPositive() {
		p.Change24h = last.Close.Sub(open).Div(open)
	}
	if f, _ := vol.Float64(); f > 0 {
		p.LogVolume24h = decimal.NewFromFloat(log10(f))
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

// barsPerDay is how many candles of a given timeframe span 24 hours.
//
// Unknown bars fall back to the 5m count rather than erroring: the caller is mid-observation and a
// slightly wrong lookback is a far smaller defect than no token profile at all. Every bar this
// project actually decides on is listed.
func barsPerDay(bar string) int {
	switch bar {
	case "1m":
		return 1440
	case "3m":
		return 480
	case "5m":
		return 288
	case "15m":
		return 96
	case "30m":
		return 48
	case "1H":
		return 24
	case "4H":
		return 6
	case "1D":
		return 1
	default:
		return 288
	}
}

// recordUpdate emits one `update` sample for a position that is still open.
//
// Without these the dataset held only open decisions and their outcomes, so the ten inputs of the
// position block — is a position open, how old, current PnL, how far it travelled each way, distance
// to each level — were ZERO on every one of 76,305 samples, while production fills them on every
// update call. That is the train/serve skew this plan exists to remove (docs/RL_V8_PLAN.md's
// "the backtest must exercise the same three, or the warm start would train a policy on a lifecycle
// different from the one it is then served").
//
// The reward is the trade's own eventual outcome, not a separate score. An `update` that says
// "keep holding" is answerable only by what the holding produced, and the reward function already
// charges the churn penalty against the adjustment count carried here (§15.5).
//
// sig is the signal to attach. A cadence-driven update passes the CARRIED signal — the last one
// this (instrument, bar) produced — exactly as lifecycle.go does via conductor.CarriedSignal: a 1H
// opinion stays meaningful for the whole hour, and dropping it the moment its candle closed would
// hide it from every update in between. Nil only when no strategy has spoken on this bar yet, where
// `present=false` is the truthful answer.
//
// An earlier version of this function sent nil for every cadence update, reasoning that a carried
// signal is stale. That produced a dataset where most updates had present=0 while production sends
// present=1 — the train/serve skew this whole plan exists to remove, reintroduced by the very code
// meant to close it.
func (r *Runner) recordUpdate(
	p *position,
	c domain.Candle,
	sig *domain.StrategySignal,
) {
	obs := p.openObs
	obs.Category = domain.CategoryUpdate
	obs.LastPrice = c.Close
	obs.AccountEquityUSD = r.account
	obs.AccountPeakUSD = r.peak
	// Assigned unconditionally, including nil: the opening observation's own signal must not leak
	// into an update as though it had just fired.
	obs.Signal = sig

	upl := pnlPct(p, c.Close)
	obs.PositionState = domain.PositionState{
		PositionOpen: true,
		Side:         sideSign(p.side),
		SizeUSD:      p.size,
		Leverage:     p.leverage,
		// From the candle's own timestamp, never wall-clock or a bar count: a replay's "now" is
		// the bar being replayed, and an elapsed-bars estimate would drift the moment a timeframe
		// has a gap (an exchange outage leaves missing candles, and the position aged through it).
		AgeSeconds:       int64(c.Timestamp.Sub(p.openedAt).Seconds()),
		UnrealizedPnLPct: upl,
		PnLMaxPct:        p.pnlMaxPct,
		PnLMinPct:        p.pnlMinPct,
		RiskPct:          riskPct(p),
		SLTPAdjustments:  p.adjustments,
	}
	if p.slPx != nil && c.Close.IsPositive() {
		obs.PositionState.DistToSLPct = p.slPx.Sub(c.Close).Div(c.Close)
	}
	if p.tpPx != nil && c.Close.IsPositive() {
		obs.PositionState.DistToTPPct = p.tpPx.Sub(c.Close).Div(c.Close)
	}

	if err := obs.Validate(); err != nil {
		r.result.Skipped["update_invalid"]++
		return
	}

	p.updates = append(p.updates, obs)
}

// signalProfile builds the signal exactly as the model receives it on an open decision.
//
// Shared by the open path and the busy-signal update path so the two cannot drift: a strategy
// firing while a position is held must reach the model in the same shape it would have on a fresh
// open, differing only in the category that frames the question.
//
// The levels are the strategy's own, unclamped — the clamp applies to an order being placed, and no
// order is placed here. Resolved so a strategy that emits only percentages still reports prices.
func (r *Runner) signalProfile(kind, bar string, sig strategy.Signal, price decimal.Decimal) *domain.StrategySignal {
	rec := r.records[kind]
	if rec == nil {
		rec = &stratRecord{}
	}
	p := rec.profile(kind, bar)
	resolved := sig.ResolveLevels(price)
	p.Side = string(sig.Side)
	p.EntryPx = resolved.EntryPx
	p.SLPx = resolved.SLPx
	p.TPPx = resolved.TPPx
	return &p
}
