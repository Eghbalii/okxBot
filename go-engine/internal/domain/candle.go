// Package domain holds plain entities shared across use-cases and adapters — no framework/IO
// imports (CLAUDE.md §10). Adapters (internal/okx, internal/postgres, ...) translate their
// wire/storage formats to and from these types.
package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// Candle is one OHLCV bar for an instrument. Timestamp is the candle's open time (UTC) — needed
// both for chart display and for any strategy whose signal depends on time-of-day/day-of-week/
// month (e.g. a Monday-open or harvest-season strategy), not just OHLCV values.
type Candle struct {
	Timestamp                      time.Time
	Open, High, Low, Close, Volume decimal.Decimal
}
