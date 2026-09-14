package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// SaveCandle upserts one OHLCV bar, keyed by (inst_id, bar, ts).
func (r *Repository) SaveCandle(ctx context.Context, c port.Candle) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO candles (inst_id, bar, ts, open, high, low, close, volume)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (inst_id, bar, ts) DO UPDATE SET
			open = EXCLUDED.open, high = EXCLUDED.high, low = EXCLUDED.low,
			close = EXCLUDED.close, volume = EXCLUDED.volume
	`, c.InstID, c.Bar, c.Timestamp, c.Open, c.High, c.Low, c.Close, c.Volume)
	if err != nil {
		return fmt.Errorf("save candle %s/%s@%s: %w", c.InstID, c.Bar, c.Timestamp, err)
	}
	return nil
}

// ListCandles returns the most recent `limit` finalized candles for instID/bar, oldest first —
// backs GET /api/candles (CLAUDE.md §16 point 6's chart) and any other consumer wanting a plain
// recent-history read of the durable candles hypertable.
func (r *Repository) ListCandles(ctx context.Context, instID, bar string, limit int) ([]port.Candle, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.pool.Query(ctx, `
		SELECT ts, open, high, low, close, volume
		FROM candles
		WHERE inst_id = $1 AND bar = $2
		ORDER BY ts DESC
		LIMIT $3
	`, instID, bar, limit)
	if err != nil {
		return nil, fmt.Errorf("list candles %s/%s: %w", instID, bar, err)
	}
	defer rows.Close()

	var out []port.Candle
	for rows.Next() {
		var c domain.Candle
		if err := rows.Scan(&c.Timestamp, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
			return nil, fmt.Errorf("scan candle: %w", err)
		}
		out = append(out, port.Candle{InstID: instID, Bar: bar, Candle: c})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Query returns newest-first (for an efficient LIMIT); reverse to oldest-first, the shape
	// every other consumer of a candle window expects (see usecase.PaperTrader.seedCandles).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListCandlesRange returns every finalized candle for instID/bar in [from, to), oldest first.
//
// The backtest's read path. It differs from ListCandles in the direction that matters: history is
// replayed FORWARD from the beginning, so the query orders ascending and pages by timestamp rather
// than returning the most recent N. A zero `from`/`to` is unbounded on that side.
func (r *Repository) ListCandlesRange(ctx context.Context, instID, bar string, from, to time.Time, limit int) ([]port.Candle, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := r.pool.Query(ctx, `
		SELECT ts, open, high, low, close, volume
		FROM candles
		WHERE inst_id = $1 AND bar = $2
		  AND ($3::timestamptz IS NULL OR ts >= $3)
		  AND ($4::timestamptz IS NULL OR ts < $4)
		ORDER BY ts ASC
		LIMIT $5
	`, instID, bar, nullTime(from), nullTime(to), limit)
	if err != nil {
		return nil, fmt.Errorf("list candles range %s/%s: %w", instID, bar, err)
	}
	defer rows.Close()

	var out []port.Candle
	for rows.Next() {
		var c domain.Candle
		if err := rows.Scan(&c.Timestamp, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
			return nil, fmt.Errorf("scan candle: %w", err)
		}
		out = append(out, port.Candle{InstID: instID, Bar: bar, Candle: c})
	}
	return out, rows.Err()
}

// CandleRange reports the oldest and newest timestamps held for instID/bar.
//
// Lets a backtest size its run without scanning the data first — and, more usefully, lets it report
// honestly how much history it actually covered rather than however much the operator asked for.
func (r *Repository) CandleRange(ctx context.Context, instID, bar string) (oldest, newest time.Time, err error) {
	var o, n *time.Time
	err = r.pool.QueryRow(ctx, `
		SELECT MIN(ts), MAX(ts) FROM candles WHERE inst_id = $1 AND bar = $2
	`, instID, bar).Scan(&o, &n)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("candle range %s/%s: %w", instID, bar, err)
	}
	if o != nil {
		oldest = *o
	}
	if n != nil {
		newest = *n
	}
	return oldest, newest, nil
}

// nullTime turns a zero time into a SQL NULL, so an unset bound means "unbounded" rather than
// "before the epoch" — which would silently exclude everything.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
