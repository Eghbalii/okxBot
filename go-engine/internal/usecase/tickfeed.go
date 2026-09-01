package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// This file is the tick/candle IO skeleton shared by PaperTrader and RealTrader (CLAUDE.md §27,
// the real-trading conductor-lifecycle plan) — mutex-guarded candle-window storage, seeding from
// Postgres at startup, and the tick/candle event decode shell. None of it makes a lifecycle
// decision (open/update/close); it only maintains "what does the market look like right now,"
// which both engines need identically. Extracted as free functions/a small candleStore type
// rather than a struct PaperTrader/RealTrader embed, specifically so neither type's existing
// field layout (and the ~40 struct-literal test constructions already built against
// PaperTrader's exact shape) needs to change — see the design note in CLAUDE.md §27's plan doc
// for why the embedding approach was tried and rejected in favor of this one.

// The candle-window state itself (a mutex + map[bar][]domain.Candle) stays as a plain pair of
// fields on each of PaperTrader/RealTrader, rather than a shared struct type — PaperTrader's
// existing tests reach directly into pt.candles/pt.candlesMu (struct-literal construction,
// direct assignment, explicit Lock/Unlock in ~10 places), so replacing those two fields with an
// embedded/nested type would force touching every one of those call sites for a refactor that is
// supposed to be behavior-preserving and low-risk. The functions below operate on that mutex+map
// pair directly (passed as parameters) so the LOGIC is shared even though the storage fields are
// not — RealTrader declares its own candlesMu/candles of the identical shape and calls the same
// functions.

// snapshotCandles returns a deep-enough copy of every maintained bar under one lock, so a
// multi-timeframe strategy sees bars consistent with each other and no lock is held across a
// Strategy.Evaluate call. Mirrors PaperTrader.marketView's exact prior behavior.
func snapshotCandles(mu *sync.Mutex, candles map[string][]domain.Candle, bar string) strategy.MarketView {
	mu.Lock()
	bars := make(map[string][]domain.Candle, len(candles))
	for b, window := range candles {
		bars[b] = append([]domain.Candle(nil), window...)
	}
	mu.Unlock()
	return strategy.MarketView{Bar: bar, Candles: bars[bar], Bars: bars}
}

// seedCandlesFromRepo fills every bar's in-memory window from candles already persisted in
// Postgres — see PaperTrader.seedCandlesFromRepo's original doc comment (CLAUDE.md's
// f1af152/2026-08-29 history) for why this reads Postgres rather than calling OKX REST. limit<=0
// is a no-op (nothing to trim into). Best-effort per bar: a read failure leaves that window
// empty, refilled from the live feed exactly as before.
func seedCandlesFromRepo(ctx context.Context, mu *sync.Mutex, candles map[string][]domain.Candle, repo port.Repository, instID string, bars []string, limit int, logger *slog.Logger) {
	if limit <= 0 {
		return
	}
	for _, bar := range bars {
		rows, err := repo.ListCandles(ctx, instID, bar, limit)
		if err != nil {
			logger.Warn("seed candles from repo failed; window will fill from the live feed",
				"instId", instID, "bar", bar, "error", err)
			continue
		}
		if len(rows) == 0 {
			continue
		}
		window := make([]domain.Candle, 0, len(rows))
		for _, r := range rows {
			window = append(window, r.Candle)
		}
		mu.Lock()
		candles[bar] = window
		mu.Unlock()
		logger.Info("seeded candle window from database", "instId", instID, "bar", bar, "candles", len(window))
	}
}

// applyCandle updates bar's window with c, replacing the last entry when it shares c's timestamp
// (OKX pushes the same forming candle repeatedly, confirm=0, then once more on close, confirm=1 —
// appending every push would fill the window with partial copies of one bar) and trimming to
// windowLimit. Mirrors PaperTrader.handleCandle's exact prior window-update logic.
func applyCandle(mu *sync.Mutex, candles map[string][]domain.Candle, bar string, c domain.Candle, windowLimit int) {
	mu.Lock()
	defer mu.Unlock()
	window := candles[bar]
	if n := len(window); n > 0 && window[n-1].Timestamp.Equal(c.Timestamp) {
		window[n-1] = c
	} else {
		window = append(window, c)
	}
	if windowLimit > 0 && len(window) > windowLimit {
		window = window[len(window)-windowLimit:]
	}
	candles[bar] = window
}

// barSeconds converts an OKX bar name ("1m", "15m", "1H", "4H", "1D", "1W") to its duration in
// seconds, for ordering timeframes shortest-to-longest. See decisionBarFor's doc comment for why
// this exists and the case-insensitivity note.
func barSeconds(bar string) int {
	if bar == "" {
		return 1 << 30
	}
	n := 0
	i := 0
	for ; i < len(bar) && bar[i] >= '0' && bar[i] <= '9'; i++ {
		n = n*10 + int(bar[i]-'0')
	}
	if n == 0 || i >= len(bar) {
		return 1 << 30
	}
	switch bar[i] {
	case 'm': // minutes — lowercase only; 'M' is months in OKX's scheme, handled below
		return n * 60
	case 'H', 'h':
		return n * 3600
	case 'D', 'd':
		return n * 86400
	case 'W', 'w':
		return n * 604800
	case 'M':
		return n * 2592000 // ~30d; only used for ordering, never for arithmetic on real timestamps
	default:
		return 1 << 30
	}
}

// decisionBarFor picks which timeframe's strategy signals and price context feed a TICK-driven RL
// decision (CLAUDE.md §15.3/§15.9) — explicit if set, otherwise the shortest configured bar (the
// freshest read of what price is doing right now). Shared by PaperTrader.decisionBar and
// RealTrader's equivalent so the "shortest bar wins" rule can't drift between the two.
func decisionBarFor(explicit string, bars []string) string {
	if explicit != "" {
		return explicit
	}
	shortest := ""
	for _, b := range bars {
		if shortest == "" || barSeconds(b) < barSeconds(shortest) {
			shortest = b
		}
	}
	return shortest
}

// tickEvent/candleEvent mirror OKX's tick/candle Kafka payload shape (CLAUDE.md §12) — shared
// decode types so PaperTrader and RealTrader parse the exact same wire format identically.
type tickEvent struct {
	InstID string `json:"instId"`
	Last   string `json:"last"`
}

type candleEvent struct {
	InstID string   `json:"instId"`
	Bar    string   `json:"bar"`
	Candle []string `json:"candle"`
}

// decodeTick parses a tick event, returning ok=false (no error) for a shared-topic message
// belonging to a different instrument — the normal, expected case, not a decode failure.
func decodeTick(data []byte, instID string) (price decimal.Decimal, ok bool, err error) {
	var tick tickEvent
	if err := json.Unmarshal(data, &tick); err != nil {
		return decimal.Decimal{}, false, fmt.Errorf("decode tick: %w", err)
	}
	if tick.InstID != instID {
		return decimal.Decimal{}, false, nil
	}
	price, err = decimal.NewFromString(tick.Last)
	if err != nil {
		return decimal.Decimal{}, false, fmt.Errorf("parse tick price %q: %w", tick.Last, err)
	}
	return price, true, nil
}

// decodedCandle is one successfully-decoded candle event, confirmed to belong to instID.
type decodedCandle struct {
	Candle    domain.Candle
	Confirmed bool // OKX's confirm=1 — the bar has closed, vs. still forming (confirm=0)
}

// decodeCandle parses a candle event, returning ok=false (no error) for a shared-topic message
// belonging to a different instrument or a malformed/too-short candle array — both expected,
// non-error cases on a shared topic.
func decodeCandle(data []byte, instID string) (dc decodedCandle, ok bool, err error) {
	var event candleEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return decodedCandle{}, false, fmt.Errorf("decode candle event: %w", err)
	}
	if event.InstID != instID || len(event.Candle) < 6 {
		return decodedCandle{}, false, nil
	}
	confirm := ""
	if len(event.Candle) >= 9 {
		confirm = event.Candle[8]
	}
	c, err := parseCandleFields(event.Candle[0], event.Candle[1], event.Candle[2], event.Candle[3], event.Candle[4], event.Candle[5])
	if err != nil {
		return decodedCandle{}, false, fmt.Errorf("parse candle (bar %s): %w", event.Bar, err)
	}
	return decodedCandle{Candle: c, Confirmed: confirm == "1"}, true, nil
}

func parseCandleFields(ts, open, high, low, close, vol string) (domain.Candle, error) {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse ts %q: %w", ts, err)
	}
	o, err := decimal.NewFromString(open)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse open %q: %w", open, err)
	}
	h, err := decimal.NewFromString(high)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse high %q: %w", high, err)
	}
	l, err := decimal.NewFromString(low)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse low %q: %w", low, err)
	}
	c, err := decimal.NewFromString(close)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse close %q: %w", close, err)
	}
	v, err := decimal.NewFromString(vol)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse vol %q: %w", vol, err)
	}
	return domain.Candle{Timestamp: time.UnixMilli(ms).UTC(), Open: o, High: h, Low: l, Close: c, Volume: v}, nil
}
