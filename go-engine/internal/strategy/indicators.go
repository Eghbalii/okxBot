package strategy

import "fmt"

// SMA returns the simple moving average of the last `period` closes. Returns an error if there
// aren't enough candles.
func SMA(candles []Candle, period int) (float64, error) {
	if len(candles) < period {
		return 0, fmt.Errorf("need at least %d candles, got %d", period, len(candles))
	}
	var sum float64
	for _, c := range candles[len(candles)-period:] {
		sum += c.Close
	}
	return sum / float64(period), nil
}

// RSI computes the Relative Strength Index over the last `period` closes using Wilder smoothing.
func RSI(candles []Candle, period int) (float64, error) {
	if len(candles) < period+1 {
		return 0, fmt.Errorf("need at least %d candles, got %d", period+1, len(candles))
	}
	window := candles[len(candles)-period-1:]

	var avgGain, avgLoss float64
	for i := 1; i < len(window); i++ {
		delta := window[i].Close - window[i-1].Close
		if delta > 0 {
			avgGain += delta
		} else {
			avgLoss += -delta
		}
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	if avgLoss == 0 {
		return 100, nil
	}
	rs := avgGain / avgLoss
	return 100 - (100 / (1 + rs)), nil
}
