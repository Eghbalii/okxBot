package domain

import "github.com/shopspring/decimal"

// MarketTicker is one instrument's 24h market snapshot as returned by an exchange's all-tickers
// endpoint — the input to token discovery (internal/usecase.MarketScanner, 2026-09-13).
//
// Deliberately NOT the same type as Ticker, which is the single-instrument trading-path shape. Two
// fields differ in a way that matters:
//
//   - Vol24hUSD is a notional in DOLLARS, normalized by each adapter. Ticker.Vol24h carries whatever
//     unit the exchange reports (OKX's volCcy24h is in base currency — 8,559,200 EDGE, not dollars;
//     MEXC's amount24 already is dollars), and ranking tokens against each other by a number whose
//     unit changes per instrument would compare EDGE-counts against BTC-counts and rank noise.
//   - Change24hPct is a percentage, likewise normalized: OKX reports open24h and leaves the caller
//     to derive it, MEXC reports riseFallRate as a fraction and has no open price at all.
//
// Putting the normalization in each adapter rather than in the scanner is what keeps the exchange's
// peculiarities at its own boundary (CLAUDE.md §10/§46's instruction that OKX's quirks must not be
// imposed on every exchange).
type MarketTicker struct {
	// InstID is the exchange's own wire-format id ("BTC-USD_UM_XPERP-310404", "BTC_USDT"). The
	// scanner derives the short internal symbol from it; the adapter does not guess.
	InstID string
	Last   decimal.Decimal
	// Open24h is zero where the exchange does not report an open price (MEXC). Read Change24hPct
	// instead of deriving change from this — see the type comment.
	Open24h      decimal.Decimal
	High24h      decimal.Decimal
	Low24h       decimal.Decimal
	Vol24hUSD    decimal.Decimal
	Change24hPct decimal.Decimal
}
