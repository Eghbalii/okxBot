package usecase

import (
	"fmt"
	"log/slog"
	"math"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// The derived indicators the model reads, and the reason this file exists.
//
// rl_service declared ten FEATURE_COLUMNS from 2026-08-26 and Go never populated the field once
// (`git log -S "tb.Features"` returns nothing). The Python replay env filled it, but that env is
// off by default (§15.8), so on the live path the model had NO RSI, NO VOLATILITY and NO VOLUME —
// it saw raw returns and two swing distances, and nothing else derived. A model asked to size a
// position and place a stop could not tell a calm market from a violent one.
//
// Computed here rather than in Python deliberately: the same indicator library the strategies use
// (internal/strategy) is the one that produces these, so there is ONE implementation of RSI in this
// system rather than two that can drift — the duplication §16.2 warns about. features.py leaves the
// live path entirely.
//
// The set differs from the old FEATURE_COLUMNS in three ways, each for a stated reason:
//   - ret_1/log_ret_1 dropped: identical to the last entry of the returns window already sent.
//     §15.11 called them redundant and the change was never applied.
//   - sma_ratio_* -> ema_ratio_*: §15.11 asked for EMA and features.py kept SMA. An EMA weights
//     recent price more, which is what a decision made on the current bar wants.
//   - atr_ratio_14 and range_ratio added: ATR is the unit the V2 strategies themselves size stops
//     in (§45), so feeding it means the model and the strategies reason in the same scale.
const (
	emaFast   = 5
	emaMid    = 10
	emaSlow   = 20
	volShort  = 5
	volMid    = 10
	volLong   = 20
	rsiPeriod = 14
	atrPeriod = 14
	volumeAvg = 20
)

// MinCandlesForIndicators is how many candles BuildIndicators needs. The binding constraint is the
// longest lookback plus one bar for the return series the volatility terms are computed from.
const MinCandlesForIndicators = volLong + 1

// BuildIndicators returns EXACTLY domain.IndicatorsPerTimeframe values, in a fixed order that
// rl_service's MarketBlock.indicators depends on positionally:
//
//	0..2  ema_ratio_5/10/20   close/EMA - 1: where price sits relative to its own trend
//	3..5  volatility_5/10/20  stddev of log returns: how violent this market currently is
//	6     rsi_14              normalized to [-1, 1] (see below)
//	7     volume_ratio_20     volume/average - 1: is this move backed by participation
//	8     atr_ratio_14        ATR/price: the unit V2 strategies size stops in
//	9     range_ratio         (high-low)/close of the live bar: intrabar conviction
//
// Returns an error rather than a partial or padded result when the window is short. That is the
// whole discipline this change introduces: a caller without enough data skips the model call.
func BuildIndicators(window []domain.Candle) ([]decimal.Decimal, error) {
	if len(window) < MinCandlesForIndicators {
		return nil, fmt.Errorf("indicators need %d candles, have %d", MinCandlesForIndicators, len(window))
	}
	last := window[len(window)-1]
	if !last.Close.IsPositive() {
		return nil, fmt.Errorf("indicators: last close is %s", last.Close)
	}
	candles := window

	out := make([]decimal.Decimal, 0, domain.IndicatorsPerTimeframe)

	for _, p := range []int{emaFast, emaMid, emaSlow} {
		ema, err := strategy.EMA(candles, p)
		if err != nil {
			return nil, fmt.Errorf("ema(%d): %w", p, err)
		}
		if !ema.IsPositive() {
			return nil, fmt.Errorf("ema(%d) is %s", p, ema)
		}
		out = append(out, last.Close.Div(ema).Sub(decimal.NewFromInt(1)))
	}

	for _, p := range []int{volShort, volMid, volLong} {
		v, err := returnVolatility(window, p)
		if err != nil {
			return nil, fmt.Errorf("volatility(%d): %w", p, err)
		}
		out = append(out, v)
	}

	rsi, err := strategy.RSI(candles, rsiPeriod)
	if err != nil {
		return nil, fmt.Errorf("rsi: %w", err)
	}
	// Normalized to [-1, 1] around 50. RSI's native 0..100 range is 100x every other input in the
	// vector, and an input on a scale that much larger dominates the first layer regardless of how
	// much information it carries — the same reason §15.11 log-compresses trade counts and converts
	// position age to hours.
	out = append(out, rsi.Sub(decimal.NewFromInt(50)).Div(decimal.NewFromInt(50)))

	avgVol, err := strategy.AvgVolume(candles, volumeAvg)
	if err != nil {
		return nil, fmt.Errorf("avg volume: %w", err)
	}
	if avgVol.IsPositive() {
		out = append(out, last.Volume.Div(avgVol).Sub(decimal.NewFromInt(1)))
	} else {
		// A genuinely zero average volume happens on a dead instrument. Zero here reads as "volume
		// is exactly average", which is the least-wrong thing to say when there is no volume to
		// compare against — and the alternative, failing the whole observation, would silence a
		// token for a reason the model does not need to care about.
		out = append(out, decimal.Zero)
	}

	atr, err := strategy.ATR(candles, atrPeriod)
	if err != nil {
		return nil, fmt.Errorf("atr: %w", err)
	}
	out = append(out, atr.Div(last.Close))

	out = append(out, last.High.Sub(last.Low).Div(last.Close))

	if len(out) != domain.IndicatorsPerTimeframe {
		// Unreachable unless the list above is edited without updating the constant, which is
		// exactly the mistake this guards: rl_service reads these positionally, so a missing value
		// would shift every later one into the wrong slot.
		return nil, fmt.Errorf("built %d indicators, want exactly %d", len(out), domain.IndicatorsPerTimeframe)
	}
	return out, nil
}

// returnVolatility is the population standard deviation of the last `period` log returns.
//
// Not strategy.StdDev: that measures dispersion of PRICE around its own mean, which scales with the
// instrument's price level and so is not comparable between a $90,000 BTC and a $0.000003 PEPE.
// Return dispersion is scale-free, which is what one shared policy across many instruments needs.
func returnVolatility(window []domain.Candle, period int) (decimal.Decimal, error) {
	if len(window) < period+1 {
		return decimal.Zero, fmt.Errorf("need %d candles, have %d", period+1, len(window))
	}
	tail := window[len(window)-(period+1):]
	rets := make([]float64, 0, period)
	for i := 1; i < len(tail); i++ {
		prev, _ := tail[i-1].Close.Float64()
		cur, _ := tail[i].Close.Float64()
		if prev <= 0 || cur <= 0 {
			return decimal.Zero, fmt.Errorf("non-positive close in volatility window")
		}
		rets = append(rets, math.Log(cur/prev))
	}
	var mean float64
	for _, r := range rets {
		mean += r
	}
	mean /= float64(len(rets))
	var sumSq float64
	for _, r := range rets {
		d := r - mean
		sumSq += d * d
	}
	return decimal.NewFromFloat(math.Sqrt(sumSq / float64(len(rets)))), nil
}

// BuildMarketBlock assembles one timeframe's full market state, or fails.
//
// Every value it produces is exact-width by construction: the returns window is EXACTLY
// domain.ReturnsWindow entries or this errors. v7's equivalent skipped any candle with a
// non-positive previous close via `continue`, which silently yielded nine returns instead of ten
// and changed the vector width — one of the things that made padding look necessary.
func BuildMarketBlock(bar string, window []domain.Candle) (domain.MarketBlock, error) {
	ind, err := BuildIndicators(window)
	if err != nil {
		return domain.MarketBlock{}, err
	}
	rets, err := BuildReturns(window)
	if err != nil {
		return domain.MarketBlock{}, err
	}

	// The last entry is the LIVE FORMING candle — handleCandle replaces rather than appends while a
	// bar is open — so this OHLC is current rather than up to a full bar stale (§15.11). In v7 all
	// four of these were built and then truncated away by to_vector on every single call.
	live := window[len(window)-1]
	mb := domain.MarketBlock{
		Bar:             bar,
		Indicators:      ind,
		Open:            live.Open,
		High:            live.High,
		Low:             live.Low,
		Close:           live.Close,
		ClosePctChanges: rets,
	}

	n := minInt(swingWindow, len(window))
	candles := window
	swingHigh, errH := strategy.Highest(candles, n)
	swingLow, errL := strategy.Lowest(candles, n)
	if errH != nil || errL != nil {
		return domain.MarketBlock{}, fmt.Errorf("swing levels: %v / %v", errH, errL)
	}
	mb.DistToSwingHighPct = swingHigh.Sub(live.Close).Div(live.Close)
	mb.DistToSwingLowPct = swingLow.Sub(live.Close).Div(live.Close)

	return mb, nil
}

// BuildReturns returns EXACTLY domain.ReturnsWindow bar-over-bar close returns, oldest first.
func BuildReturns(window []domain.Candle) ([]decimal.Decimal, error) {
	if len(window) < domain.ReturnsWindow+1 {
		return nil, fmt.Errorf("returns need %d candles, have %d", domain.ReturnsWindow+1, len(window))
	}
	tail := window[len(window)-(domain.ReturnsWindow+1):]
	out := make([]decimal.Decimal, 0, domain.ReturnsWindow)
	for i := 1; i < len(tail); i++ {
		prev := tail[i-1].Close
		if !prev.IsPositive() {
			// Errors rather than skipping: skipping is what produced a variable-length window, and
			// a non-positive close is a data fault worth seeing, not working around.
			return nil, fmt.Errorf("non-positive close at index %d", i-1)
		}
		out = append(out, tail[i].Close.Sub(prev).Div(prev))
	}
	return out, nil
}

// BuildBTCContext assembles the market-wide reference block from BTC's own candle window.
//
// window is BTC's DECISION-BAR window — everything except Correlation is built from it, because
// "what is BTC doing right now" needs the freshest read (the last entry is the live forming
// candle, CLAUDE.md's own reasoning for why this block exists at all).
//
// corrTokenReturns/corrBTCReturns are a SEPARATE pair of return series used only for the
// correlation term (2026-09-21 change) — deliberately allowed to be on a DIFFERENT, longer
// timeframe than `window`. 10 bars of a fast decision timeframe (e.g. 5m) is under an hour of data,
// too short a window for "does this token currently follow BTC" to mean much; correlation is
// measured one timeframe up instead (see nextHigherBar), while the rest of this block stays on the
// live decision bar. Pass nil for either to leave Correlation at zero (no higher bar configured
// yet, a token with no window yet, or BTC itself).
func BuildBTCContext(window []domain.Candle, corrTokenReturns, corrBTCReturns []decimal.Decimal) (domain.BTCContext, error) {
	rets, err := BuildReturns(window)
	if err != nil {
		return domain.BTCContext{}, fmt.Errorf("btc: %w", err)
	}
	live := window[len(window)-1]
	if !live.Close.IsPositive() {
		return domain.BTCContext{}, fmt.Errorf("btc: last close is %s", live.Close)
	}

	ctx := domain.BTCContext{
		Open:            live.Open,
		High:            live.High,
		Low:             live.Low,
		Close:           live.Close,
		ClosePctChanges: rets,
	}

	n := minInt(swingWindow, len(window))
	candles := window
	if swingHigh, err := strategy.Highest(candles, n); err == nil {
		ctx.DistToSwingHighPct = swingHigh.Sub(live.Close).Div(live.Close)
	}
	if swingLow, err := strategy.Lowest(candles, n); err == nil {
		ctx.DistToSwingLowPct = swingLow.Sub(live.Close).Div(live.Close)
	}

	ctx.Correlation = correlation(corrTokenReturns, corrBTCReturns)
	return ctx, nil
}

// correlation is the Pearson correlation of two equal-length return series, in [-1, 1].
//
// Fed to the model explicitly rather than left to be inferred from the two series: at this
// project's trade volume the policy would never learn to compute a correlation, and "is this token
// currently following BTC" is precisely the question the operator described mattering — a token
// can be cleanly trending and reverse the moment BTC turns red.
//
// Zero when either series is flat, which is the honest answer: with no variation there is no
// relationship to measure, and it is also what an uncorrelated pair scores.
func correlation(a, b []decimal.Decimal) decimal.Decimal {
	if len(a) == 0 || len(a) != len(b) {
		return decimal.Zero
	}
	xs := make([]float64, len(a))
	ys := make([]float64, len(b))
	var mx, my float64
	for i := range a {
		xs[i], _ = a[i].Float64()
		ys[i], _ = b[i].Float64()
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(len(xs))
	my /= float64(len(ys))

	var cov, vx, vy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx <= 0 || vy <= 0 {
		return decimal.Zero
	}
	r := cov / math.Sqrt(vx*vy)
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return decimal.Zero
	}
	return decimal.NewFromFloat(math.Max(-1, math.Min(1, r)))
}

// btcCorrelationReturns resolves the pair of return series BuildBTCContext's Correlation term is
// measured over — deliberately ONE TIMEFRAME UP from `bar`, not `bar` itself (2026-09-21 operator
// request). 10 bars of a 5m decision timeframe is under an hour of data, too short a window for
// "does this token currently follow BTC" to mean anything; the same 10-bar window measured one step
// up (e.g. 15m, or 1H if trading itself later moves to 15m) covers proportionally more real time
// without needing a wider ReturnsWindow that every OTHER use of BuildReturns would then also carry.
//
// "One step up" is resolved dynamically via nextHigherBar against `available` — the bar set this
// instrument is CURRENTLY maintaining (a MarketView's own Bars keys) — never a hardcoded timeframe
// name. If the decision timeframe changes later, or a new higher bar is added to
// paper_trading.bars/ingestion.bars, this tracks it automatically with no code change.
//
// Falls back to the decision-bar series themselves (today's pre-2026-09-21 behavior, passed in as
// decisionTokenReturns/decisionBTCReturns) when no higher bar is configured or its window hasn't
// filled yet — a warm-up/config gap should degrade the correlation's time horizon, not remove the
// feature outright by erroring the whole observation.
func btcCorrelationReturns(
	bar string,
	decisionTokenReturns, decisionBTCReturns []decimal.Decimal,
	tokenBars map[string][]domain.Candle,
	btcCandles func(string) ([]domain.Candle, bool),
) (tokenReturns, btcReturns []decimal.Decimal) {
	fallback := func() ([]decimal.Decimal, []decimal.Decimal) { return decisionTokenReturns, decisionBTCReturns }
	if btcCandles == nil {
		return fallback()
	}
	available := make([]string, 0, len(tokenBars))
	for b := range tokenBars {
		available = append(available, b)
	}
	higher := nextHigherBar(bar, available)
	if higher == "" {
		return fallback()
	}
	tokenWindow, ok := tokenBars[higher]
	if !ok {
		return fallback()
	}
	tokenRets, err := BuildReturns(tokenWindow)
	if err != nil {
		return fallback()
	}
	btcWindow, ok := btcCandles(higher)
	if !ok || len(btcWindow) == 0 {
		return fallback()
	}
	btcRets, err := BuildReturns(btcWindow)
	if err != nil {
		return fallback()
	}
	return tokenRets, btcRets
}

// skipModelCall records a decision where the model was deliberately NOT consulted because its
// observation could not be built (docs/RL_V8_PLAN.md).
//
// Logged AND counted, because §16.9's lesson is that a path which declines to act leaves no trace
// unless something is written to make it visible — there, the model was consulted on all 522
// signals, answered unusably every time, and the metric stayed empty, which read as "the model was
// never called". A quiet model and a broken feed must not look identical.
//
// Debug rather than Warn on the log: the common case is a short candle window in the minutes after
// a restart, which is correct behaviour and would otherwise fill the log with alarming lines about
// something working as designed. The metric is the durable signal.
func (e *PaperTrader) skipModelCall(stage string, err error, logger *slog.Logger) {
	reason := domain.ValidationField(err)
	if reason == "" {
		reason = "build"
	}
	metrics.ModelCallsSkippedTotal.WithLabelValues(e.InstID, stage, reason).Inc()
	logger.Debug("rl: observation unusable, skipping model call",
		"stage", stage, "reason", reason, "error", err)
}

// skipModelCall is BotTrader's own copy, deliberately not shared with PaperTrader's.
//
// The two engines keep separate implementations of their lifecycle throughout (§27.3), and a shared
// method would need an interface for one logging call. The duplication §23 warns about is a field
// dropped from a struct literal; this is four lines with no state.
func (e *BotTrader) skipModelCall(stage string, err error, logger *slog.Logger) {
	reason := domain.ValidationField(err)
	if reason == "" {
		reason = "build"
	}
	metrics.ModelCallsSkippedTotal.WithLabelValues(e.InstID, stage, reason).Inc()
	logger.Debug("rl: observation unusable, skipping model call",
		"stage", stage, "reason", reason, "error", err)
}
