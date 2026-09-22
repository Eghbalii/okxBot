package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// This is the exact collision migration 000038 exists to prevent (2026-09-22 multi-exchange
// ingestion work): before it, `candles` was keyed by (inst_id, bar, ts) alone, so a second
// exchange's ingestor (MEXC) publishing under the same short internal symbol ("BTC") would silently
// UPSERT over OKX's own row for that instId/bar/ts. Runs against a REAL database or not at all —
// same "tests here run against a real database or skip cleanly" posture as
// paper_trading_config_test.go, since a mocked version of this test would pass against the exact
// bug it exists to catch (Postgres's ON CONFLICT target is what actually enforces the isolation,
// and no fake can reproduce that).
func TestSaveCandle_ExchangeScopingPreventsCollision(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	instID := "TEST_CANDLE_SCOPE_BTC"
	bar := "5m"
	ts := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(ctx, `DELETE FROM candles WHERE inst_id = $1 AND bar = $2`, instID, bar)
	})

	okxCandle := domain.Candle{
		Timestamp: ts,
		Open:      decimal.RequireFromString("100"),
		High:      decimal.RequireFromString("110"),
		Low:       decimal.RequireFromString("90"),
		Close:     decimal.RequireFromString("105"),
		Volume:    decimal.RequireFromString("1000"),
	}
	mexcCandle := domain.Candle{
		Timestamp: ts,
		Open:      decimal.RequireFromString("999"),
		High:      decimal.RequireFromString("999"),
		Low:       decimal.RequireFromString("999"),
		Close:     decimal.RequireFromString("999"),
		Volume:    decimal.RequireFromString("999"),
	}

	// Save the SAME instId/bar/ts under two different exchanges. Before migration 000038, the
	// second SaveCandle here would have overwritten the first's row outright (ON CONFLICT
	// (inst_id, bar, ts)) — this is precisely what was found live: a MEXC paper-trader instance
	// reading OKX's own BTC/ETH candle history because nothing distinguished the two.
	if err := repo.SaveCandle(ctx, port.Candle{InstID: instID, Bar: bar, Exchange: "okx", Candle: okxCandle}); err != nil {
		t.Fatalf("save okx candle: %v", err)
	}
	if err := repo.SaveCandle(ctx, port.Candle{InstID: instID, Bar: bar, Exchange: "mexc", Candle: mexcCandle}); err != nil {
		t.Fatalf("save mexc candle: %v", err)
	}

	okxRows, err := repo.ListCandles(ctx, "okx", instID, bar, 10)
	if err != nil {
		t.Fatalf("list okx candles: %v", err)
	}
	if len(okxRows) != 1 {
		t.Fatalf("okx candles: want 1 row, got %d", len(okxRows))
	}
	if !okxRows[0].Close.Equal(decimal.RequireFromString("105")) {
		t.Errorf("okx candle close: want 105 (unclobbered by the mexc save), got %s — the exchange scoping collided", okxRows[0].Close)
	}

	mexcRows, err := repo.ListCandles(ctx, "mexc", instID, bar, 10)
	if err != nil {
		t.Fatalf("list mexc candles: %v", err)
	}
	if len(mexcRows) != 1 {
		t.Fatalf("mexc candles: want 1 row, got %d", len(mexcRows))
	}
	if !mexcRows[0].Close.Equal(decimal.RequireFromString("999")) {
		t.Errorf("mexc candle close: want 999, got %s", mexcRows[0].Close)
	}
}

// An existing caller that never sets Exchange (every pre-2026-09-22 call site) must keep reading
// and writing exactly what it always did — "okx" — with no config/code change required. This is
// the zero-behavior-change guarantee migration 000038's own header comment promises.
func TestSaveCandle_EmptyExchangeDefaultsToOKX(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	instID := "TEST_CANDLE_SCOPE_DEFAULT"
	bar := "5m"
	ts := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(ctx, `DELETE FROM candles WHERE inst_id = $1 AND bar = $2`, instID, bar)
	})

	c := domain.Candle{
		Timestamp: ts,
		Open:      decimal.RequireFromString("1"),
		High:      decimal.RequireFromString("2"),
		Low:       decimal.RequireFromString("1"),
		Close:     decimal.RequireFromString("1.5"),
		Volume:    decimal.RequireFromString("10"),
	}
	// Exchange deliberately left unset, exactly as every call site did before 2026-09-22.
	if err := repo.SaveCandle(ctx, port.Candle{InstID: instID, Bar: bar, Candle: c}); err != nil {
		t.Fatalf("save candle with no exchange set: %v", err)
	}

	// Reading with exchange="" (an existing caller's own default) must find it...
	rows, err := repo.ListCandles(ctx, "", instID, bar, 10)
	if err != nil {
		t.Fatalf("list candles (exchange=\"\"): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row with exchange=\"\", got %d", len(rows))
	}

	// ...and so must reading with exchange="okx" explicitly — the two must be the same row, not two
	// different defaults that happen to both work.
	okxRows, err := repo.ListCandles(ctx, "okx", instID, bar, 10)
	if err != nil {
		t.Fatalf("list candles (exchange=okx): %v", err)
	}
	if len(okxRows) != 1 {
		t.Fatalf("want 1 row with exchange=okx, got %d", len(okxRows))
	}

	// A different exchange must NOT see this row.
	mexcRows, err := repo.ListCandles(ctx, "mexc", instID, bar, 10)
	if err != nil {
		t.Fatalf("list candles (exchange=mexc): %v", err)
	}
	if len(mexcRows) != 0 {
		t.Errorf("mexc must not see okx's candle row, got %d rows", len(mexcRows))
	}
}

// ListCandlesRange/CandleRange must apply the same exchange scoping as ListCandles — the backtest
// read path and the panel's oldest/newest range query, respectively.
func TestListCandlesRangeAndCandleRange_ExchangeScoped(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	instID := "TEST_CANDLE_SCOPE_RANGE"
	bar := "5m"
	ts1 := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 9, 22, 10, 5, 0, 0, time.UTC)
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(ctx, `DELETE FROM candles WHERE inst_id = $1 AND bar = $2`, instID, bar)
	})

	mk := func(ts time.Time, close string) domain.Candle {
		return domain.Candle{
			Timestamp: ts,
			Open:      decimal.RequireFromString("1"),
			High:      decimal.RequireFromString("2"),
			Low:       decimal.RequireFromString("1"),
			Close:     decimal.RequireFromString(close),
			Volume:    decimal.RequireFromString("10"),
		}
	}

	if err := repo.SaveCandle(ctx, port.Candle{InstID: instID, Bar: bar, Exchange: "mexc", Candle: mk(ts1, "10")}); err != nil {
		t.Fatalf("save mexc candle 1: %v", err)
	}
	if err := repo.SaveCandle(ctx, port.Candle{InstID: instID, Bar: bar, Exchange: "mexc", Candle: mk(ts2, "20")}); err != nil {
		t.Fatalf("save mexc candle 2: %v", err)
	}
	// Same instId/bar under okx, deliberately different timestamps — proves the range/oldest-newest
	// queries filter by exchange rather than merely happening to return the right count by luck.
	if err := repo.SaveCandle(ctx, port.Candle{InstID: instID, Bar: bar, Exchange: "okx", Candle: mk(ts1.Add(-time.Hour), "999")}); err != nil {
		t.Fatalf("save okx candle: %v", err)
	}

	rangeRows, err := repo.ListCandlesRange(ctx, "mexc", instID, bar, time.Time{}, time.Time{}, 100)
	if err != nil {
		t.Fatalf("list candles range: %v", err)
	}
	if len(rangeRows) != 2 {
		t.Fatalf("mexc range: want 2 rows, got %d", len(rangeRows))
	}

	oldest, newest, err := repo.CandleRange(ctx, "mexc", instID, bar)
	if err != nil {
		t.Fatalf("candle range: %v", err)
	}
	if !oldest.Equal(ts1) {
		t.Errorf("mexc oldest: want %s, got %s (leaked okx's earlier row?)", ts1, oldest)
	}
	if !newest.Equal(ts2) {
		t.Errorf("mexc newest: want %s, got %s", ts2, newest)
	}
}
