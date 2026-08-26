package strategy

import "github.com/shopspring/decimal"

// TrendConfluence is a Go port of the core signal logic from "TrendMaster Pro 2.3 with Alerts"
// (pinescript/strategy_TrendMaster Pro 2.3 with Alerts.pine, no stated license). The original is
// mostly display tooling (Band Power, session boxes/highlighting, Support & Resistance pivots,
// alert-message plumbing) — none of that is signal logic and none of it is ported. What's ported
// is the actual entry filter chain: a fast/slow MA crossover gated by five independent
// confirmation filters that must ALL agree (Bollinger volatility+trend, RSI, MACD, Stochastic,
// ADX strength) — the source's own thresholds for these are hardcoded ("Fixed Values for Hidden
// Settings" in the original UI, not user-exposed), so they're ported as fixed strategy defaults,
// tunable like everything else here. The higher-timeframe trend filter (a second MA the source
// calls "trendMA") is included since it's simple confluence, same as the others. Exit is ATR-
// based: SL = ATRMultiplierSL * ATR from entry, TP = RiskReward multiples of that SL distance —
// directly from the source's `_sl`/`_tp` helpers.
type TrendConfluence struct {
	ShortMALength int
	LongMALength  int

	EnableTrendFilter bool
	TrendMALength     int

	BBLength     int
	BBMultiplier decimal.Decimal

	RSILength   int
	RSILongMin  decimal.Decimal
	RSIShortMax decimal.Decimal

	MACDFast   int
	MACDSlow   int
	MACDSignal int

	StochLength     int
	StochSmoothing  int
	StochOversold   decimal.Decimal
	StochOverbought decimal.Decimal

	ADXLength    int
	ADXThreshold decimal.Decimal

	ATRLength       int
	ATRMultiplierSL decimal.Decimal
	RiskReward      decimal.Decimal

	prevShortMA, prevLongMA decimal.Decimal
	hasPrev                 bool
}

func NewTrendConfluence() *TrendConfluence {
	return &TrendConfluence{
		ShortMALength: 9,
		LongMALength:  21,

		EnableTrendFilter: false,
		TrendMALength:     50,

		BBLength:     20,
		BBMultiplier: decimal.NewFromFloat(2.0),

		RSILength:   14,
		RSILongMin:  decimal.NewFromInt(55),
		RSIShortMax: decimal.NewFromInt(45),

		MACDFast:   12,
		MACDSlow:   26,
		MACDSignal: 9,

		StochLength:     14,
		StochSmoothing:  3,
		StochOversold:   decimal.NewFromInt(20),
		StochOverbought: decimal.NewFromInt(80),

		ADXLength:    14,
		ADXThreshold: decimal.NewFromInt(25),

		ATRLength:       14,
		ATRMultiplierSL: decimal.NewFromFloat(2.0),
		RiskReward:      decimal.NewFromFloat(2.0),
	}
}

func (s *TrendConfluence) Name() string { return "trend_confluence" }

func (s *TrendConfluence) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "short_ma_length", Default: decimal.NewFromInt(int64(s.ShortMALength)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "long_ma_length", Default: decimal.NewFromInt(int64(s.LongMALength)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "rsi_long_min", Default: s.RSILongMin, Min: decimal.NewFromInt(50), Max: decimal.NewFromInt(90)},
		{Name: "rsi_short_max", Default: s.RSIShortMax, Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(50)},
		{Name: "adx_threshold", Default: s.ADXThreshold, Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(60)},
		{Name: "atr_multiplier_sl", Default: s.ATRMultiplierSL, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(10)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(10)},
	}
}

func (s *TrendConfluence) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["short_ma_length"]; ok {
		cp.ShortMALength = int(ClampParam(specByName["short_ma_length"], v).IntPart())
	}
	if v, ok := values["long_ma_length"]; ok {
		cp.LongMALength = int(ClampParam(specByName["long_ma_length"], v).IntPart())
	}
	if v, ok := values["rsi_long_min"]; ok {
		cp.RSILongMin = ClampParam(specByName["rsi_long_min"], v)
	}
	if v, ok := values["rsi_short_max"]; ok {
		cp.RSIShortMax = ClampParam(specByName["rsi_short_max"], v)
	}
	if v, ok := values["adx_threshold"]; ok {
		cp.ADXThreshold = ClampParam(specByName["adx_threshold"], v)
	}
	if v, ok := values["atr_multiplier_sl"]; ok {
		cp.ATRMultiplierSL = ClampParam(specByName["atr_multiplier_sl"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	return &cp
}

func stdDev(candles []Candle, period int) (decimal.Decimal, error) {
	mean, err := SMA(candles, period)
	if err != nil {
		return decimal.Zero, err
	}
	window := candles[len(candles)-period:]
	sumSq := decimal.Zero
	for _, c := range window {
		d := c.Close.Sub(mean)
		sumSq = sumSq.Add(d.Mul(d))
	}
	variance := sumSq.Div(decimal.NewFromInt(int64(period)))
	return variance.Pow(decimal.NewFromFloat(0.5)), nil
}

func macdLines(candles []Candle, fast, slow, signal int) (macd, signalLine decimal.Decimal, err error) {
	fastEMA, err := EMASeries(candles, fast)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	slowEMA, err := EMASeries(candles, slow)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	macdSeries := make([]decimal.Decimal, len(candles))
	for i := slow - 1; i < len(candles); i++ {
		macdSeries[i] = fastEMA[i].Sub(slowEMA[i])
	}
	// EMA of the MACD series itself, over its valid tail, for the signal line.
	validMACD := macdSeries[slow-1:]
	if len(validMACD) < signal {
		return decimal.Zero, decimal.Zero, errNeedMore(slow-1+signal, len(candles))
	}
	alpha := decimal.NewFromInt(2).Div(decimal.NewFromInt(int64(signal + 1)))
	oneMinusAlpha := decimal.NewFromInt(1).Sub(alpha)
	sum := decimal.Zero
	for i := 0; i < signal; i++ {
		sum = sum.Add(validMACD[i])
	}
	sigEMA := sum.Div(decimal.NewFromInt(int64(signal)))
	for i := signal; i < len(validMACD); i++ {
		sigEMA = validMACD[i].Mul(alpha).Add(sigEMA.Mul(oneMinusAlpha))
	}
	return macdSeries[len(macdSeries)-1], sigEMA, nil
}

// directionalMovement computes Wilder's +DI/-DI and DX (the un-smoothed single-point predecessor
// to ADX: |+DI - -DI| / (+DI + -DI) * 100) over `period`, recomputed from the trailing window
// every call rather than carrying incremental RMA state across calls. True ADX is DX smoothed by
// another `period`-length RMA over time; that would need to be tracked as receiver state (like
// GridLike's baseline) to avoid recomputing an ever-growing DX history on every candle. DX is a
// reasonable approximation of trend strength for a threshold filter (higher DX ~ stronger trend,
// same interpretation as ADX) but will be noisier bar-to-bar than the fully smoothed indicator.
func directionalMovement(candles []Candle, period int) (plusDI, minusDI, adx decimal.Decimal, err error) {
	if len(candles) < period*2+1 {
		return decimal.Zero, decimal.Zero, decimal.Zero, errNeedMore(period*2+1, len(candles))
	}
	window := candles[len(candles)-period*2-1:]

	rma := func(values []decimal.Decimal) decimal.Decimal {
		periodDec := decimal.NewFromInt(int64(period))
		sum := decimal.Zero
		for _, v := range values[:period] {
			sum = sum.Add(v)
		}
		r := sum.Div(periodDec)
		for _, v := range values[period:] {
			r = r.Mul(periodDec.Sub(decimal.NewFromInt(1))).Add(v).Div(periodDec)
		}
		return r
	}

	trueRanges := make([]decimal.Decimal, 0, len(window)-1)
	plusDMs := make([]decimal.Decimal, 0, len(window)-1)
	minusDMs := make([]decimal.Decimal, 0, len(window)-1)
	for i := 1; i < len(window); i++ {
		hl := window[i].High.Sub(window[i].Low)
		hc := window[i].High.Sub(window[i-1].Close).Abs()
		lc := window[i].Low.Sub(window[i-1].Close).Abs()
		tr := hl
		if hc.GreaterThan(tr) {
			tr = hc
		}
		if lc.GreaterThan(tr) {
			tr = lc
		}
		trueRanges = append(trueRanges, tr)

		upMove := window[i].High.Sub(window[i-1].High)
		downMove := window[i-1].Low.Sub(window[i].Low)
		plusDM, minusDM := decimal.Zero, decimal.Zero
		if upMove.GreaterThan(downMove) && upMove.IsPositive() {
			plusDM = upMove
		}
		if downMove.GreaterThan(upMove) && downMove.IsPositive() {
			minusDM = downMove
		}
		plusDMs = append(plusDMs, plusDM)
		minusDMs = append(minusDMs, minusDM)
	}

	atrRMA := rma(trueRanges)
	if atrRMA.IsZero() {
		return decimal.Zero, decimal.Zero, decimal.Zero, nil
	}
	plusDI = rma(plusDMs).Div(atrRMA).Mul(hundred)
	minusDI = rma(minusDMs).Div(atrRMA).Mul(hundred)

	diSum := plusDI.Add(minusDI)
	if diSum.IsZero() {
		return plusDI, minusDI, decimal.Zero, nil
	}
	dx := plusDI.Sub(minusDI).Abs().Div(diSum).Mul(hundred)
	return plusDI, minusDI, dx, nil // dx used as an ADX approximation (single-point, not RMA-smoothed further)
}

func (s *TrendConfluence) Evaluate(candles []Candle) (Signal, error) {
	need := s.LongMALength
	for _, n := range []int{s.TrendMALength, s.BBLength, s.RSILength + 1, s.MACDSlow + s.MACDSignal, s.StochLength + s.StochSmoothing*2, s.ADXLength*2 + 1, s.ATRLength + 1} {
		if n > need {
			need = n
		}
	}
	if len(candles) < need+1 {
		return Signal{Side: Hold}, nil
	}

	shortMANow, err := EMA(candles, s.ShortMALength)
	if err != nil {
		return Signal{}, err
	}
	longMANow, err := EMA(candles, s.LongMALength)
	if err != nil {
		return Signal{}, err
	}
	if !s.hasPrev {
		s.prevShortMA, s.prevLongMA, s.hasPrev = shortMANow, longMANow, true
		return Signal{Side: Hold}, nil
	}
	prevShortMA, prevLongMA := s.prevShortMA, s.prevLongMA
	s.prevShortMA, s.prevLongMA = shortMANow, longMANow

	crossedUp := prevShortMA.LessThanOrEqual(prevLongMA) && shortMANow.GreaterThan(longMANow)
	crossedDown := prevShortMA.GreaterThanOrEqual(prevLongMA) && shortMANow.LessThan(longMANow)
	if !crossedUp && !crossedDown {
		return Signal{Side: Hold}, nil
	}

	close := candles[len(candles)-1].Close

	trendFilterLong, trendFilterShort := true, true
	if s.EnableTrendFilter {
		trendMA, err := EMA(candles, s.TrendMALength)
		if err != nil {
			return Signal{}, err
		}
		trendFilterLong = close.GreaterThan(trendMA)
		trendFilterShort = close.LessThan(trendMA)
	}

	basis, err := SMA(candles, s.BBLength)
	if err != nil {
		return Signal{}, err
	}
	dev, err := stdDev(candles, s.BBLength)
	if err != nil {
		return Signal{}, err
	}
	upperBand := basis.Add(s.BBMultiplier.Mul(dev))
	lowerBand := basis.Sub(s.BBMultiplier.Mul(dev))
	bbAtr, err := ATR(candles, s.BBLength)
	if err != nil {
		return Signal{}, err
	}
	volatilityFilter := upperBand.Sub(lowerBand).GreaterThan(bbAtr.Mul(s.BBMultiplier))
	bbTrendFilterLong := close.GreaterThan(basis)
	bbTrendFilterShort := close.LessThan(basis)

	rsi, err := RSI(candles, s.RSILength)
	if err != nil {
		return Signal{}, err
	}
	rsiFilterLong := rsi.GreaterThan(s.RSILongMin)
	rsiFilterShort := rsi.LessThan(s.RSIShortMax)

	macd, signalLine, err := macdLines(candles, s.MACDFast, s.MACDSlow, s.MACDSignal)
	if err != nil {
		return Signal{}, err
	}
	macdFilterLong := macd.GreaterThan(signalLine)
	macdFilterShort := macd.LessThan(signalLine)

	kNow := smaSeries(rawStochK(candles, s.StochLength), s.StochSmoothing)
	dLine := smaSeries(kNow, s.StochSmoothing)
	last := len(candles) - 1
	stochFilterLong := kNow[last].GreaterThan(s.StochOversold) && kNow[last].GreaterThan(dLine[last])
	stochFilterShort := kNow[last].LessThan(s.StochOverbought) && kNow[last].LessThan(dLine[last])

	_, _, adx, err := directionalMovement(candles, s.ADXLength)
	if err != nil {
		return Signal{}, err
	}
	adxFilter := adx.GreaterThan(s.ADXThreshold)

	atr, err := ATR(candles, s.ATRLength)
	if err != nil {
		return Signal{}, err
	}
	slDist := atr.Mul(s.ATRMultiplierSL)
	if close.IsZero() {
		return Signal{Side: Hold}, nil
	}
	slPct := slDist.Div(close)
	tpPct := slPct.Mul(s.RiskReward)

	buy := crossedUp && volatilityFilter && bbTrendFilterLong && rsiFilterLong && macdFilterLong && stochFilterLong && adxFilter && trendFilterLong
	sell := crossedDown && volatilityFilter && bbTrendFilterShort && rsiFilterShort && macdFilterShort && stochFilterShort && adxFilter && trendFilterShort

	switch {
	case buy:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.7), SLPct: slPct, TPPct: tpPct}, nil
	case sell:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.7), SLPct: slPct, TPPct: tpPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
