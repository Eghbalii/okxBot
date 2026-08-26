package postgres

import (
	"context"
	"fmt"

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
	`, c.InstID, c.Bar, c.Ts, c.Open, c.High, c.Low, c.Close, c.Volume)
	if err != nil {
		return fmt.Errorf("save candle %s/%s@%s: %w", c.InstID, c.Bar, c.Ts, err)
	}
	return nil
}
