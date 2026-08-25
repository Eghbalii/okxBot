package strategy

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// SMA returns the simple moving average of the last `period` closes. Returns an error if there
// aren't enough candles.
func SMA(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, fmt.Errorf("need at least %d candles, got %d", period, len(candles))
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
		return decimal.Zero, fmt.Errorf("need at least %d candles, got %d", period+1, len(candles))
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
