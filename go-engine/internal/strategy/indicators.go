package strategy

import (
	"fmt"

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
		out[i] = candles[i].Close.Mul(alpha).Add(out[i-1].Mul(oneMinusAlpha))
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
