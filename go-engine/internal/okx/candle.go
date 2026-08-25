package okx

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/rez/okxBot/go-engine/internal/domain"
)

// Candle is one OHLCV bar from OKX's REST /market/candles (or WS candle channel), still as the
// raw string fields OKX returns — OKX's candle arrays are positionally decoded ([]string), so
// this stays string-typed at the wire boundary; use ToDomain to parse.
type Candle struct {
	Ts      string // ms epoch timestamp, candle open time
	Open    string
	High    string
	Low     string
	Close   string
	Vol     string
	Confirm string // "0" = still forming, "1" = finalized
}

// ToDomain parses the raw OHLCV string fields into a domain.Candle.
func (c Candle) ToDomain() (domain.Candle, error) {
	open, err := decimal.NewFromString(c.Open)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse open %q: %w", c.Open, err)
	}
	high, err := decimal.NewFromString(c.High)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse high %q: %w", c.High, err)
	}
	low, err := decimal.NewFromString(c.Low)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse low %q: %w", c.Low, err)
	}
	closePx, err := decimal.NewFromString(c.Close)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse close %q: %w", c.Close, err)
	}
	vol, err := decimal.NewFromString(c.Vol)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse vol %q: %w", c.Vol, err)
	}
	return domain.Candle{Open: open, High: high, Low: low, Close: closePx, Volume: vol}, nil
}
