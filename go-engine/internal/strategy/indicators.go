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
