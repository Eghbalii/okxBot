// Package domain holds plain entities shared across use-cases and adapters — no framework/IO
// imports (CLAUDE.md §10). Adapters (internal/okx, internal/postgres, ...) translate their
// wire/storage formats to and from these types.
package domain

import "github.com/shopspring/decimal"

// Candle is one OHLCV bar for an instrument.
type Candle struct {
	Open, High, Low, Close, Volume decimal.Decimal
}
