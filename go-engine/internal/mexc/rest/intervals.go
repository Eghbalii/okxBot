package rest

import "time"

// MEXC names timeframes "Min5"/"Hour4"/"Day1" where this project uses OKX-style "5m"/"4H"/"1D"
// internally. The translation lives here, at the adapter boundary, so no caller outside
// internal/mexc ever needs to know MEXC's spelling.
//
// This is deliberately a lookup with an ok-flag rather than string manipulation. CLAUDE.md §9
// records what the alternative costs: a mis-cased OKX bar ("1h" for "1H") subscribed to a channel
// that pushed nothing, so that timeframe silently produced no candles while every log looked
// healthy. An unknown bar must fail loudly at the call, never be passed through and hope.
//
// Verified live 2026-09-13: a bogus interval returns {"success":false,"code":600,"message":
// "Parameter error"} — which is a clear failure, but only if the request is made at all. The point
// of this table is to fail before that, with a message naming the bar.
//
// "1H" is "Min60", NOT "Hour1" (corrected 2026-09-22, found by tools/candlefetch's first real run
// against the live REST kline endpoint) — "Hour1" returns the exact code=600 "Parameter error"
// above on EVERY request, with or without start/end, confirmed by curling
// contract.mexc.com/api/v1/contract/kline/BTC_USDT directly. This table's original "1H":"Hour1"
// entry was apparently never actually exercised against the live REST endpoint (only against the
// separate WS subscription path, cmd/ingestor/mexc.go's own wsIntervalFor, which already uses the
// correct "Min60" — that override now exists ONLY for historical/documentation reasons, since REST
// and WS agree on "Min60" for this one bar after this fix; wsIntervalFor is left in place rather
// than removed, since deleting it would silently reintroduce a REST/WS naming assumption if this
// table's own "1H" entry is ever changed again without re-verifying WS separately).
var intervals = map[string]string{
	"1m":  "Min1",
	"5m":  "Min5",
	"15m": "Min15",
	"30m": "Min30",
	"1H":  "Min60",
	"4H":  "Hour4",
	"8H":  "Hour8",
	"1D":  "Day1",
	"1W":  "Week1",
	"1M":  "Month1",
}

// barDurations is how long one bar of each timeframe lasts, used to derive a start time for
// endpoints that take a window rather than a count.
//
// Kept as a separate table from intervals, and every key here has a counterpart there — enforced by
// a test rather than by convention, since a bar present in one map and missing from the other
// degrades quietly (the candle request still works, it just fetches an unintended window).
var barDurations = map[string]time.Duration{
	"1m":  time.Minute,
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1H":  time.Hour,
	"4H":  4 * time.Hour,
	"8H":  8 * time.Hour,
	"1D":  24 * time.Hour,
	"1W":  7 * 24 * time.Hour,
	"1M":  30 * 24 * time.Hour,
}

// IntervalFor translates an internal bar name into MEXC's interval vocabulary. ok is false for an
// unsupported bar, which callers must treat as an error rather than defaulting.
func IntervalFor(bar string) (string, bool) {
	v, ok := intervals[bar]
	return v, ok
}

// BarDuration returns how long one bar of the given timeframe lasts.
func BarDuration(bar string) (time.Duration, bool) {
	d, ok := barDurations[bar]
	return d, ok
}

// SupportedBars lists every internal bar name this adapter can serve, for startup validation.
func SupportedBars() []string {
	out := make([]string, 0, len(intervals))
	for k := range intervals {
		out = append(out, k)
	}
	return out
}
