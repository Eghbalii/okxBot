package main

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	mexcrest "github.com/eghbalii/okxBot/go-engine/internal/mexc/rest"
)

// TestMexcCandleArray_BuildsTheExactLayoutDecodeCandleExpects proves mexcCandleArray's array
// matches internal/usecase.decodeCandle's exact expected shape: index 0=ts_ms (as a decimal
// string, NOT seconds — domain.Candle.Timestamp round-trips through UnixMilli), 1-5=OHLCV, 8="1"
// (confirmed). See internal/usecase/tickfeed_test.go's
// TestDecodeCandle_ParsesMEXCShapedArray for the other half of this same contract, proven by
// running the REAL decodeCandle against this exact array shape — that test and this one together
// are what make the wire contract genuinely round-tripped rather than each side's own guess about
// what the other expects.
func TestMexcCandleArray_BuildsTheExactLayoutDecodeCandleExpects(t *testing.T) {
	ts := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	c := domain.Candle{
		Timestamp: ts,
		Open:      decimal.RequireFromString("100.5"),
		High:      decimal.RequireFromString("101"),
		Low:       decimal.RequireFromString("99.5"),
		Close:     decimal.RequireFromString("100.75"),
		Volume:    decimal.RequireFromString("12345.6789"),
	}

	got := mexcCandleArray(c)

	if len(got) != 9 {
		t.Fatalf("want a 9-element array (indices 0-8), got %d elements: %v", len(got), got)
	}
	wantTS := "1790078400000" // ts.UnixMilli()
	if got[0] != wantTS {
		t.Errorf("index 0 (ts_ms): want %s, got %s", wantTS, got[0])
	}
	if got[1] != "100.5" {
		t.Errorf("index 1 (open): want 100.5, got %s", got[1])
	}
	if got[2] != "101" {
		t.Errorf("index 2 (high): want 101, got %s", got[2])
	}
	if got[3] != "99.5" {
		t.Errorf("index 3 (low): want 99.5, got %s", got[3])
	}
	if got[4] != "100.75" {
		t.Errorf("index 4 (close): want 100.75, got %s", got[4])
	}
	if got[5] != "12345.6789" {
		t.Errorf("index 5 (volume): want 12345.6789, got %s", got[5])
	}
	if got[8] != "1" {
		t.Errorf("index 8 (confirm): want \"1\" (always finalized), got %q", got[8])
	}
}

// TestMexcCandleArray_TimestampIsMilliseconds guards against the specific off-by-a-factor-of-1000
// mistake this conversion invites: MEXC's own KlinePush carries `t` in SECONDS (see
// internal/mexc/ws/payload.go's own doc comment), but domain.Candle.Timestamp is a time.Time, and
// decodeCandle/parseCandleFields parse index 0 as MILLISECONDS
// (strconv.ParseInt + time.UnixMilli). Using c.Timestamp.Unix() here instead of .UnixMilli() would
// compile, look plausible, and silently timestamp every MEXC candle 1000x too early.
func TestMexcCandleArray_TimestampIsMilliseconds(t *testing.T) {
	ts := time.Unix(1700000000, 0).UTC() // a round, recognizable Unix-seconds value
	c := domain.Candle{Timestamp: ts, Open: decimal.Zero, High: decimal.Zero, Low: decimal.Zero, Close: decimal.Zero, Volume: decimal.Zero}

	got := mexcCandleArray(c)
	if got[0] != "1700000000000" {
		t.Fatalf("index 0 must be milliseconds (1700000000000), got %s — a seconds/milliseconds mixup would produce 1700000000", got[0])
	}
}

// TestMexcCandleArray_IndicesSixAndSevenAreEmpty documents (and pins) that OKX's volCcy/
// volCcyQuote fields, which MEXC has no equivalent of, are left blank rather than omitted —
// decodeCandle never reads them (only checks len(...) for the fields it does read), so this is
// safe, but a future reader should not assume a non-empty value belongs there.
func TestMexcCandleArray_IndicesSixAndSevenAreEmpty(t *testing.T) {
	got := mexcCandleArray(domain.Candle{Timestamp: time.Now(), Open: decimal.Zero, High: decimal.Zero, Low: decimal.Zero, Close: decimal.Zero, Volume: decimal.Zero})
	if got[6] != "" || got[7] != "" {
		t.Errorf("indices 6-7 (volCcy/volCcyQuote, no MEXC equivalent): want empty, got %q, %q", got[6], got[7])
	}
}

// TestBarToMEXCInterval_EveryDecisionBarConfiguredForThisProjectResolves guards the specific
// bars this project actually configures (config.mexc.yaml's ingestion.bars: 5m/15m/1H/4H/1D) — a
// regression here would silently drop one of the currently-collected timeframes from the MEXC
// ingestor without any code change elsewhere ever flagging it.
func TestBarToMEXCInterval_EveryDecisionBarConfiguredForThisProjectResolves(t *testing.T) {
	for _, bar := range []string{"5m", "15m", "1H", "4H", "1D"} {
		if _, ok := mexcrest.IntervalFor(bar); !ok {
			t.Errorf("bar %q (configured in config.mexc.yaml's ingestion.bars) has no MEXC interval mapping", bar)
		}
	}
}

// TestBarToMEXCInterval_UnsupportedBarIsReportedNotSilentlySkipped proves the exact failure mode
// the ingestor's own bar loop must handle: an unsupported bar's ok=false, which runMEXCIngestor
// treats as "warn and skip this one bar" rather than subscribing to a channel that pushes nothing
// (the §9 "loud failure over a silent data gap" rule this whole codebase follows). This test pins
// IntervalFor's own contract; runMEXCIngestor's loop behavior around it is a straight-line `if !ok
// { logger.Warn(...); continue }` with no further branching to unit-test in isolation.
func TestBarToMEXCInterval_UnsupportedBarIsReportedNotSilentlySkipped(t *testing.T) {
	if _, ok := mexcrest.IntervalFor("3m"); ok {
		t.Fatal("\"3m\" has no MEXC equivalent (see internal/mexc/rest/intervals.go's table) — expected ok=false")
	}
}
