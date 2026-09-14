package strategy

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// Measures the real ADX distribution on this project's own instruments and timeframe, so the regime
// thresholds come from the market being traded rather than from the TradingView author's defaults —
// written for daily charts, where ADX behaves very differently from 5m.
//
// Skips when the sample files are absent, so it never blocks a normal test run.
func TestADXDistributionOnRealCandles(t *testing.T) {
	files := map[string]string{"SOL": "/tmp/sol5m.csv", "BTC": "/tmp/btc5m.csv"}
	report := ""
	for name, path := range files {
		cs, err := loadCSVCandles(path)
		if err != nil || len(cs) < 200 {
			t.Skipf("no sample candles at %s", path)
		}
		var vals []float64
		for i := 60; i < len(cs); i++ {
			v, err := ADX(cs[i-60:i], 14)
			if err != nil {
				continue
			}
			f, _ := v.Float64()
			vals = append(vals, f)
		}
		if len(vals) == 0 {
			t.Fatalf("%s: no ADX values computed", name)
		}
		sort.Float64s(vals)
		q := func(p float64) float64 { return vals[int(p*float64(len(vals)-1))] }
		above := func(th float64) float64 {
			n := 0
			for _, v := range vals {
				if v >= th {
					n++
				}
			}
			return 100 * float64(n) / float64(len(vals))
		}
		report += fmt.Sprintf("%s n=%d p25=%.1f p50=%.1f p75=%.1f p90=%.1f | >=20:%.0f%% >=25:%.0f%% >=30:%.0f%% >=35:%.0f%%\n",
			name, len(vals), q(.25), q(.50), q(.75), q(.90), above(20), above(25), above(30), above(35))
	}
	os.WriteFile("/tmp/adxdist.txt", []byte(report), 0644)
}

func loadCSVCandles(path string) ([]Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	var out []Candle
	// The export is newest-first; every consumer of a candle window expects oldest-first.
	for i := len(rows) - 1; i >= 0; i-- {
		rec := rows[i]
		if len(rec) < 6 {
			continue
		}
		d := func(s string) decimal.Decimal {
			v, _ := decimal.NewFromString(strings.TrimSpace(s))
			return v
		}
		out = append(out, Candle{
			Open: d(rec[1]), High: d(rec[2]), Low: d(rec[3]), Close: d(rec[4]), Volume: d(rec[5]),
		})
	}
	return out, nil
}
