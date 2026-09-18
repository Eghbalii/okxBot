package strategy

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"
)

// errNeedMore is the standard "not enough candles" error shared by indicator functions.
func errNeedMore(need, got int) error {
	return fmt.Errorf("need at least %d candles, got %d", need, got)
}

// SMA returns the simple moving average of the last `period` closes. Returns an error if there
// aren't enough candles.
func SMA(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	sum := decimal.Zero
	for _, c := range candles[len(candles)-period:] {
		sum = sum.Add(c.Close)
	}
	return sum.Div(decimal.NewFromInt(int64(period))), nil
}

// RSI computes the Relative Strength Index over the last `period` closes using Wilder smoothing.
func RSI(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period+1 {
		return decimal.Zero, errNeedMore(period+1, len(candles))
	}
	window := candles[len(candles)-period-1:]

	avgGain, avgLoss := decimal.Zero, decimal.Zero
	for i := 1; i < len(window); i++ {
		delta := window[i].Close.Sub(window[i-1].Close)
		if delta.IsPositive() {
			avgGain = avgGain.Add(delta)
		} else {
			avgLoss = avgLoss.Add(delta.Neg())
		}
	}
	periodDec := decimal.NewFromInt(int64(period))
	avgGain = avgGain.Div(periodDec)
	avgLoss = avgLoss.Div(periodDec)

	if avgLoss.IsZero() {
		return decimal.NewFromInt(100), nil
	}
	rs := avgGain.Div(avgLoss)
	return decimal.NewFromInt(100).Sub(decimal.NewFromInt(100).Div(decimal.NewFromInt(1).Add(rs))), nil
}

// EMASeries returns the exponential moving average of candle closes over `period`, one value per
// candle from index period-1 onward (index period-2 and earlier are the zero value). Unlike SMA/
// RSI/ATR, callers that need EMA's recursive history (crossovers, chained EMAs like DEMA) want the
// whole series, not just the latest value.
// emaScale bounds EMA's recursive accumulator — see the rounding note in EMASeries. 12 decimal
// places is well past the precision of any instrument this bot trades while keeping the value from
// growing a fixed number of digits per candle forever.
const emaScale = 12

func EMASeries(candles []Candle, period int) ([]decimal.Decimal, error) {
	if len(candles) < period {
		return nil, errNeedMore(period, len(candles))
	}
	out := make([]decimal.Decimal, len(candles))
	alpha := decimal.NewFromInt(2).Div(decimal.NewFromInt(int64(period + 1)))
	oneMinusAlpha := decimal.NewFromInt(1).Sub(alpha)

	sum := decimal.Zero
	for i := 0; i < period; i++ {
		sum = sum.Add(candles[i].Close)
	}
	out[period-1] = sum.Div(decimal.NewFromInt(int64(period)))

	for i := period; i < len(candles); i++ {
		// Round each step. alpha is a repeating decimal for most periods (2/11 at period 10), and
		// decimal.Decimal is arbitrary-precision, so multiplying the running value by
		// oneMinusAlpha on every bar compounds ~16 new digits per candle with nothing to truncate
		// it: measured 498 digits at 40 candles, 4,659 at 300, growing without bound for as long as
		// the window does. Those values reach NUMERIC columns and every model observation, so this
		// is not merely untidy.
		//
		// emaScale is far beyond the significance of any real price while keeping the value
		// bounded; rounding the accumulator (not just the result) is what stops the growth, since
		// the previous value is what feeds the next multiplication.
		out[i] = candles[i].Close.Mul(alpha).Add(out[i-1].Mul(oneMinusAlpha)).Round(emaScale)
	}
	return out, nil
}

// EMA returns the latest exponential moving average of closes over `period`.
func EMA(candles []Candle, period int) (decimal.Decimal, error) {
	series, err := EMASeries(candles, period)
	if err != nil {
		return decimal.Zero, err
	}
	return series[len(series)-1], nil
}

// ATR computes the Average True Range over `period` using Wilder (RMA) smoothing.
func ATR(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period+1 {
		return decimal.Zero, errNeedMore(period+1, len(candles))
	}
	trueRange := func(i int) decimal.Decimal {
		hl := candles[i].High.Sub(candles[i].Low)
		hc := candles[i].High.Sub(candles[i-1].Close).Abs()
		lc := candles[i].Low.Sub(candles[i-1].Close).Abs()
		tr := hl
		if hc.GreaterThan(tr) {
			tr = hc
		}
		if lc.GreaterThan(tr) {
			tr = lc
		}
		return tr
	}

	start := len(candles) - period - 1
	periodDec := decimal.NewFromInt(int64(period))

	sum := decimal.Zero
	for i := start + 1; i <= start+period; i++ {
		sum = sum.Add(trueRange(i))
	}
	atr := sum.Div(periodDec)
	for i := start + period + 1; i < len(candles); i++ {
		atr = atr.Mul(periodDec.Sub(decimal.NewFromInt(1))).Add(trueRange(i)).Div(periodDec)
	}
	return atr, nil
}

// Highest returns the highest High over the last `period` candles.
func Highest(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	window := candles[len(candles)-period:]
	highest := window[0].High
	for _, c := range window {
		if c.High.GreaterThan(highest) {
			highest = c.High
		}
	}
	return highest, nil
}

// Lowest returns the lowest Low over the last `period` candles.
func Lowest(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	window := candles[len(candles)-period:]
	lowest := window[0].Low
	for _, c := range window {
		if c.Low.LessThan(lowest) {
			lowest = c.Low
		}
	}
	return lowest, nil
}

// StdDev returns the population standard deviation of the last `period` closes around their own
// SMA — the building block for Bollinger Bands.
func StdDev(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
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
	f, _ := variance.Float64()
	return decimal.NewFromFloat(math.Sqrt(f)), nil
}

// BollingerBands returns (basis, upper, lower) using an SMA basis of `period` closes and bands at
// `mult` standard deviations.
func BollingerBands(candles []Candle, period int, mult decimal.Decimal) (basis, upper, lower decimal.Decimal, err error) {
	basis, err = SMA(candles, period)
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	sd, err := StdDev(candles, period)
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	band := sd.Mul(mult)
	return basis, basis.Add(band), basis.Sub(band), nil
}

// SessionVWAP returns the volume-weighted average price over the last `period` candles — a
// rolling VWAP rather than a calendar-session-anchored one, since this codebase's candle windows
// aren't segmented by exchange session boundaries. Falls back to a plain average price when the
// window's total volume is zero (a quiet instrument/timeframe), so callers never divide by zero.
func SessionVWAP(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	window := candles[len(candles)-period:]
	sumPV := decimal.Zero
	sumV := decimal.Zero
	for _, c := range window {
		typical := c.High.Add(c.Low).Add(c.Close).Div(decimal.NewFromInt(3))
		sumPV = sumPV.Add(typical.Mul(c.Volume))
		sumV = sumV.Add(c.Volume)
	}
	if sumV.IsZero() {
		return SMA(candles, period)
	}
	return sumPV.Div(sumV), nil
}

// KeltnerChannel returns (middle, upper, lower): an EMA midline of `period` closes with bands at
// `mult` ATRs (of the same period) — a smoother, volatility-scaled alternative to Bollinger Bands.
func KeltnerChannel(candles []Candle, period int, mult decimal.Decimal) (middle, upper, lower decimal.Decimal, err error) {
	middle, err = EMA(candles, period)
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	atr, err := ATR(candles, period)
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	band := atr.Mul(mult)
	return middle, middle.Add(band), middle.Sub(band), nil
}

// AvgVolume returns the average Volume over the last `period` candles.
func AvgVolume(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	window := candles[len(candles)-period:]
	sum := decimal.Zero
	for _, c := range window {
		sum = sum.Add(c.Volume)
	}
	return sum.Div(decimal.NewFromInt(int64(period))), nil
}

// ADX measures TREND STRENGTH without regard to direction, over `period` bars.
//
// Added 2026-09-14 for the regime-adaptive strategies. It answers a question no indicator in this
// package could: not "which way is price going" but "is it going anywhere at all". Screening 41
// strategies found none of them asking that — they all trade their pattern whenever it appears, and
// a breakout pattern in a chopping market is noise no matter how cleanly it forms.
//
// Wilder's formulation: directional movement is the portion of a bar's range that extends beyond
// the previous bar's, smoothed; ADX is the smoothed absolute difference between the two directional
// indicators as a fraction of their sum. Conventionally above ~25 reads as trending and below ~20
// as ranging, but those thresholds are instrument- and timeframe-dependent and belong in a
// strategy's parameters rather than here.
//
// Uses Wilder's smoothing (an EMA with alpha = 1/period), matching every published ADX; a simple
// moving average would produce a different, more reactive number under the same name.
func ADX(candles []Candle, period int) (decimal.Decimal, error) {
	// Two smoothing passes are needed — one for DI, one for ADX itself — so the series must be long
	// enough for both to warm up, not just for one.
	if period < 1 {
		period = 14
	}
	if len(candles) < 2*period+1 {
		return decimal.Zero, errNeedMore(2*period+1, len(candles))
	}

	dxs, err := dxSeries(candles, period)
	if err != nil {
		return decimal.Zero, err
	}
	if len(dxs) < period {
		return decimal.Zero, errNeedMore(period, len(dxs))
	}

	// Seed the ADX with the mean of the first `period` DX values, then smooth the rest — Wilder's
	// own initialization, and what makes this comparable to the number a charting package shows.
	sum := decimal.Zero
	for _, d := range dxs[:period] {
		sum = sum.Add(d)
	}
	adx := sum.Div(decimal.NewFromInt(int64(period)))
	n := decimal.NewFromInt(int64(period))
	for _, d := range dxs[period:] {
		adx = adx.Mul(n.Sub(decimal.NewFromInt(1))).Add(d).Div(n)
	}
	return adx.Round(emaScale), nil
}

// DirectionalIndex returns (+DI, -DI) — the directional halves ADX summarizes.
//
// Exposed separately because a strategy that knows the market is trending usually also wants to
// know which way, and recomputing the smoothing to get it would double the work for a number
// already calculated here.
func DirectionalIndex(candles []Candle, period int) (plusDI, minusDI decimal.Decimal, err error) {
	if period < 1 {
		period = 14
	}
	if len(candles) < period+1 {
		return decimal.Zero, decimal.Zero, errNeedMore(period+1, len(candles))
	}
	p, m, _, err := smoothedDM(candles, period, len(candles)-1)
	return p, m, err
}

// dxSeries computes the DX value at every bar where it is defined.
func dxSeries(candles []Candle, period int) ([]decimal.Decimal, error) {
	var out []decimal.Decimal
	for i := period; i < len(candles); i++ {
		plusDI, minusDI, ok, err := smoothedDM(candles, period, i)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		sum := plusDI.Add(minusDI)
		if !sum.IsPositive() {
			// Both directional indicators zero: no movement at all in the window. DX is undefined,
			// and zero is the honest reading — nothing is trending.
			out = append(out, decimal.Zero)
			continue
		}
		out = append(out, plusDI.Sub(minusDI).Abs().Div(sum).Mul(decimal.NewFromInt(100)))
	}
	return out, nil
}

// smoothedDM returns Wilder-smoothed +DI and -DI as of bar `at`.
func smoothedDM(candles []Candle, period, at int) (plusDI, minusDI decimal.Decimal, ok bool, err error) {
	if at < period || at >= len(candles) {
		return decimal.Zero, decimal.Zero, false, nil
	}

	var trSum, plusSum, minusSum decimal.Decimal
	for i := at - period + 1; i <= at; i++ {
		tr, up, down := directionalMove(candles[i-1], candles[i])
		trSum = trSum.Add(tr)
		plusSum = plusSum.Add(up)
		minusSum = minusSum.Add(down)
	}
	if !trSum.IsPositive() {
		return decimal.Zero, decimal.Zero, true, nil
	}
	hundred := decimal.NewFromInt(100)
	return plusSum.Div(trSum).Mul(hundred).Round(emaScale),
		minusSum.Div(trSum).Mul(hundred).Round(emaScale), true, nil
}

// WMASeries returns the linearly-weighted moving average of closes over `period`, one value per
// candle from index period-1 onward (earlier indices are the zero value) — the building block HMA
// needs for its own recursive weighting. Weight increases linearly toward the most recent close in
// the window, same convention as pmax.go's own wma() but exposed as a full series since HMA needs
// two different WMA lengths computed over the same window at every bar, not just the latest value.
func WMASeries(candles []Candle, period int) ([]decimal.Decimal, error) {
	if len(candles) < period {
		return nil, errNeedMore(period, len(candles))
	}
	out := make([]decimal.Decimal, len(candles))
	weightTotal := decimal.NewFromInt(int64(period * (period + 1) / 2))
	for i := period - 1; i < len(candles); i++ {
		window := candles[i-period+1 : i+1]
		weightedSum := decimal.Zero
		for j, c := range window {
			weightedSum = weightedSum.Add(c.Close.Mul(decimal.NewFromInt(int64(j + 1))))
		}
		out[i] = weightedSum.Div(weightTotal)
	}
	return out, nil
}

// HMASeries returns the Hull Moving Average over `period`, one value per candle from the point
// enough history exists onward (earlier indices are the zero value).
//
// HMA = WMA(2*WMA(close, period/2) - WMA(close, period), round(sqrt(period))) — Alan Hull's
// published construction: it halves lag by extrapolating a fast WMA past a slow one, then smooths
// that extrapolation with a further WMA over a shorter window. Building the whole series (not just
// the latest value) is what a slope/turning-point strategy needs, since "did it turn" requires
// comparing consecutive HMA values computed the same way.
func HMASeries(candles []Candle, period int) ([]decimal.Decimal, error) {
	if period < 2 {
		period = 2
	}
	halfLen := period / 2
	if halfLen < 1 {
		halfLen = 1
	}
	sqrtLen := int(math.Round(math.Sqrt(float64(period))))
	if sqrtLen < 1 {
		sqrtLen = 1
	}
	if len(candles) < period {
		return nil, errNeedMore(period, len(candles))
	}

	wmaHalf, err := WMASeries(candles, halfLen)
	if err != nil {
		return nil, err
	}
	wmaFull, err := WMASeries(candles, period)
	if err != nil {
		return nil, err
	}

	// raw[i] = 2*WMA(half) - WMA(full), valid from index period-1 onward (WMA(full) is the longer of
	// the two and gates when both are defined).
	raw := make([]Candle, len(candles))
	for i := period - 1; i < len(candles); i++ {
		v := wmaHalf[i].Mul(decimal.NewFromInt(2)).Sub(wmaFull[i])
		raw[i] = Candle{Close: v}
	}
	validRaw := raw[period-1:]
	if len(validRaw) < sqrtLen {
		return nil, errNeedMore(period+sqrtLen-1, len(candles))
	}
	smoothed, err := WMASeries(validRaw, sqrtLen)
	if err != nil {
		return nil, err
	}

	out := make([]decimal.Decimal, len(candles))
	for i, v := range smoothed {
		if v.IsZero() && i < sqrtLen-1 {
			continue // still zero-valued warm-up from WMASeries' own convention
		}
		out[period-1+i] = v
	}
	return out, nil
}

// HMA returns the latest Hull Moving Average over `period`.
func HMA(candles []Candle, period int) (decimal.Decimal, error) {
	series, err := HMASeries(candles, period)
	if err != nil {
		return decimal.Zero, err
	}
	last := series[len(series)-1]
	if last.IsZero() {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	return last, nil
}

// Ichimoku returns the Tenkan-sen (conversion line) and Kijun-sen (base line): the midpoint of the
// highest-high/lowest-low over `tenkanPeriod` and `kijunPeriod` bars respectively — Goichi
// Hosoda's published construction (standard defaults 9/26). Only the two lines a TK-cross strategy
// needs are computed; the cloud (Senkou spans, plotted 26 bars forward) and Chikou span are display
// elements this codebase's Strategy interface has no forward-plotting concept for for and aren't
// needed by a TK-cross signal.
func Ichimoku(candles []Candle, tenkanPeriod, kijunPeriod int) (tenkan, kijun decimal.Decimal, err error) {
	tenkanHigh, err := Highest(candles, tenkanPeriod)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	tenkanLow, err := Lowest(candles, tenkanPeriod)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	kijunHigh, err := Highest(candles, kijunPeriod)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	kijunLow, err := Lowest(candles, kijunPeriod)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	two := decimal.NewFromInt(2)
	tenkan = tenkanHigh.Add(tenkanLow).Div(two)
	kijun = kijunHigh.Add(kijunLow).Div(two)
	return tenkan, kijun, nil
}

// Stochastic returns the classic %K/%D oscillator: %K is smoothed by kSmooth (1 = raw %K, the
// "fast" stochastic; >1 = "slow" stochastic, George Lane's own recommended default 3), %D is a
// further period-length SMA of %K. Reuses stoch_cross.go's own rawStochK/smaSeries series builders
// so the two implementations of the same indicator can't drift.
func Stochastic(candles []Candle, kPeriod, kSmooth, dPeriod int) (k, d decimal.Decimal, err error) {
	need := kPeriod + kSmooth + dPeriod
	if len(candles) < need {
		return decimal.Zero, decimal.Zero, errNeedMore(need, len(candles))
	}
	rawK := rawStochK(candles, kPeriod)
	kLine := smaSeries(rawK, kSmooth)
	dLine := smaSeries(kLine, dPeriod)
	last := len(candles) - 1
	return kLine[last], dLine[last], nil
}

// ParabolicSARState is one bar's SAR state, carried forward by the caller between calls — SAR is
// inherently recursive (the next bar's dot depends on this one's trend, extreme point, and
// acceleration factor), so unlike the other indicators here it cannot be recomputed standalone from
// a single call without replaying the whole series. ParabolicSARSeries below does that replay once;
// a stateful strategy may instead carry a ParabolicSARState across calls if replaying the full
// window on every candle becomes a real cost.
type ParabolicSARState struct {
	SAR          decimal.Decimal
	Uptrend      bool
	ExtremePoint decimal.Decimal
	AF           decimal.Decimal
}

// ParabolicSARSeries computes Welles Wilder's Parabolic SAR over the whole candle series, returning
// one SAR value and trend-direction flag per candle from index 1 onward (index 0 has no prior bar
// to seed from and is the zero value / false). afStart/afStep/afMax are Wilder's own published
// defaults when passed 0.02/0.02/0.2.
func ParabolicSARSeries(candles []Candle, afStart, afStep, afMax decimal.Decimal) ([]decimal.Decimal, []bool, error) {
	if len(candles) < 2 {
		return nil, nil, errNeedMore(2, len(candles))
	}
	sars := make([]decimal.Decimal, len(candles))
	trends := make([]bool, len(candles))

	// Seed from the first two bars: trend is up if the second bar's close rose, SAR starts at the
	// first bar's low (uptrend) or high (downtrend) — Wilder's own initialization.
	uptrend := candles[1].Close.GreaterThanOrEqual(candles[0].Close)
	var sar, ep decimal.Decimal
	if uptrend {
		sar = candles[0].Low
		ep = candles[1].High
	} else {
		sar = candles[0].High
		ep = candles[1].Low
	}
	af := afStart
	sars[1], trends[1] = sar, uptrend

	for i := 2; i < len(candles); i++ {
		prevSAR := sar
		nextSAR := prevSAR.Add(af.Mul(ep.Sub(prevSAR)))

		if uptrend {
			// SAR must never sit inside the prior two bars' range.
			if nextSAR.GreaterThan(candles[i-1].Low) {
				nextSAR = candles[i-1].Low
			}
			if i >= 2 && nextSAR.GreaterThan(candles[i-2].Low) {
				nextSAR = candles[i-2].Low
			}
			if candles[i].Low.LessThan(nextSAR) {
				// Flip to downtrend: SAR resets to the prior extreme point, AF resets.
				uptrend = false
				nextSAR = ep
				ep = candles[i].Low
				af = afStart
			} else if candles[i].High.GreaterThan(ep) {
				ep = candles[i].High
				af = af.Add(afStep)
				if af.GreaterThan(afMax) {
					af = afMax
				}
			}
		} else {
			if nextSAR.LessThan(candles[i-1].High) {
				nextSAR = candles[i-1].High
			}
			if i >= 2 && nextSAR.LessThan(candles[i-2].High) {
				nextSAR = candles[i-2].High
			}
			if candles[i].High.GreaterThan(nextSAR) {
				uptrend = true
				nextSAR = ep
				ep = candles[i].High
				af = afStart
			} else if candles[i].Low.LessThan(ep) {
				ep = candles[i].Low
				af = af.Add(afStep)
				if af.GreaterThan(afMax) {
					af = afMax
				}
			}
		}
		sar = nextSAR
		sars[i], trends[i] = sar, uptrend
	}
	return sars, trends, nil
}

// ZigZagPivot is one detected swing point.
type ZigZagPivot struct {
	Index int
	Price decimal.Decimal
	High  bool // true = swing high, false = swing low
}

// ZigZagPivots finds local swing highs/lows using a simple `depth`-bar fractal: a candle whose High
// is the highest of the `depth` bars on each side is a swing high, mirrored for lows. This is the
// standard, well-known "fractal" pivot definition (the same shape TradingView's own ZigZag/Williams
// Fractals indicators use), not the percentage-reversal ZigZag variant — a fractal needs no
// reversal-percentage parameter and reacts to structure alone, which is what a swing-anchored
// strategy wants to key off.
//
// Only pivots with `depth` confirmed bars on both sides are returned, so every result is final (a
// pivot near the end of the series that hasn't yet been confirmed on its right side is correctly
// omitted rather than guessed at).
func ZigZagPivots(candles []Candle, depth int) []ZigZagPivot {
	if depth < 1 {
		depth = 2
	}
	var out []ZigZagPivot
	for i := depth; i < len(candles)-depth; i++ {
		isHigh, isLow := true, true
		for j := i - depth; j <= i+depth; j++ {
			if j == i {
				continue
			}
			if candles[j].High.GreaterThanOrEqual(candles[i].High) {
				isHigh = false
			}
			if candles[j].Low.LessThanOrEqual(candles[i].Low) {
				isLow = false
			}
		}
		if isHigh {
			out = append(out, ZigZagPivot{Index: i, Price: candles[i].High, High: true})
		}
		if isLow {
			out = append(out, ZigZagPivot{Index: i, Price: candles[i].Low, High: false})
		}
	}
	return out
}

// AnchoredVWAP returns the volume-weighted average price computed from `fromIndex` (inclusive)
// through the end of candles — a VWAP anchored to a specific bar (typically a significant swing
// high/low) rather than a fixed rolling window, matching how a discretionary trader draws an
// anchored VWAP from a chart pivot. Falls back to a plain average when the anchored window's total
// volume is zero, same convention as SessionVWAP.
func AnchoredVWAP(candles []Candle, fromIndex int) (decimal.Decimal, error) {
	if fromIndex < 0 || fromIndex >= len(candles) {
		return decimal.Zero, fmt.Errorf("anchor index %d out of range [0,%d)", fromIndex, len(candles))
	}
	window := candles[fromIndex:]
	sumPV, sumV := decimal.Zero, decimal.Zero
	for _, c := range window {
		typical := c.High.Add(c.Low).Add(c.Close).Div(decimal.NewFromInt(3))
		sumPV = sumPV.Add(typical.Mul(c.Volume))
		sumV = sumV.Add(c.Volume)
	}
	if sumV.IsZero() {
		sum := decimal.Zero
		for _, c := range window {
			sum = sum.Add(c.Close)
		}
		return sum.Div(decimal.NewFromInt(int64(len(window)))), nil
	}
	return sumPV.Div(sumV), nil
}

// directionalMove returns one bar's true range and its directional movement.
//
// Only the LARGER of the two moves counts, and only when it is positive: a bar that extends beyond
// the previous one in both directions is an expansion, not a directional move, and counting both
// would read an inside-out bar as simultaneously bullish and bearish.
func directionalMove(prev, cur Candle) (tr, up, down decimal.Decimal) {
	hl := cur.High.Sub(cur.Low)
	hc := cur.High.Sub(prev.Close).Abs()
	lc := cur.Low.Sub(prev.Close).Abs()
	tr = hl
	if hc.GreaterThan(tr) {
		tr = hc
	}
	if lc.GreaterThan(tr) {
		tr = lc
	}

	upMove := cur.High.Sub(prev.High)
	downMove := prev.Low.Sub(cur.Low)
	if upMove.IsPositive() && upMove.GreaterThan(downMove) {
		up = upMove
	}
	if downMove.IsPositive() && downMove.GreaterThan(upMove) {
		down = downMove
	}
	return tr, up, down
}
