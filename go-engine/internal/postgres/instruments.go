package postgres

import (
	"context"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

const instrumentCols = `id, symbol, exchange, exec_inst_id, inst_type,
	enabled_ingest, enabled_paper, enabled_real, source,
	coalesce(vol_24h_usd, 0), coalesce(change_24h_pct, 0), coalesce(scan_score, 0),
	created_at, updated_at`

// instrumentWhere builds f's Exchange/Enabled predicate, shared by ListInstruments and
// CountInstruments so the two can never disagree about which rows a filter matches — a paginated
// list and its page-count query silently drifting apart is worse than either alone, since the
// panel would show a page count that does not match what paging through the list actually finds.
//
// The enabled filter maps a consumer name onto its own column rather than taking a column name
// from the caller — a caller-supplied identifier would be either an injection surface or a silent
// no-op on a typo, and there are exactly three legal values.
func instrumentWhere(f port.InstrumentFilter) (where string, args []any, err error) {
	if f.Exchange != "" {
		args = append(args, f.Exchange)
		where += fmt.Sprintf(" AND exchange = $%d", len(args))
	}
	switch f.Enabled {
	case "":
	case "ingest":
		where += " AND enabled_ingest"
	case "paper":
		where += " AND enabled_paper"
	case "bot":
		where += " AND enabled_real"
	default:
		return "", nil, fmt.Errorf("unknown enabled filter %q", f.Enabled)
	}
	return where, args, nil
}

// ListInstruments reads the tradeable-instrument roster (migration 000031). Ordered by symbol so
// the panel's roster view and each service's startup log are stable between reads rather than
// following Postgres's physical row order.
func (r *Repository) ListInstruments(ctx context.Context, f port.InstrumentFilter) ([]port.Instrument, error) {
	where, args, err := instrumentWhere(f)
	if err != nil {
		return nil, fmt.Errorf("list instruments: %w", err)
	}

	query := `SELECT ` + instrumentCols + ` FROM instruments WHERE true` + where + ` ORDER BY symbol`
	if f.Limit > 0 {
		args = append(args, f.Limit, f.Offset)
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	}

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list instruments: %w", err)
	}
	defer rows.Close()

	var out []port.Instrument
	for rows.Next() {
		var in port.Instrument
		if err := rows.Scan(&in.ID, &in.Symbol, &in.Exchange, &in.ExecInstID, &in.InstType,
			&in.EnabledIngest, &in.EnabledPaper, &in.EnabledReal, &in.Source,
			&in.Vol24hUSD, &in.Change24hPct, &in.ScanScore,
			&in.CreatedAt, &in.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan instrument: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// CountInstruments returns how many rows f's Exchange/Enabled filters match, ignoring
// f.Limit/f.Offset — see the port interface's doc comment for why this exists as its own method
// rather than a COUNT(*) OVER() window column on ListInstruments (matches CountPositions' own
// precedent elsewhere in this file).
func (r *Repository) CountInstruments(ctx context.Context, f port.InstrumentFilter) (int, error) {
	where, args, err := instrumentWhere(f)
	if err != nil {
		return 0, fmt.Errorf("count instruments: %w", err)
	}
	var n int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM instruments WHERE true`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count instruments: %w", err)
	}
	return n, nil
}

// UpsertInstrument adds a roster row or refreshes an existing one, keyed by (exchange, symbol).
//
// An existing row's THREE ENABLE FLAGS ARE DELIBERATELY NOT OVERWRITTEN. A scan re-finding a token
// it admitted days ago must not resurrect one an operator has since disabled — that is migration
// 000030's exact bug (a service overruling a person's choice because it could not tell that choice
// from its own earlier one), and the same mistake is available here. Only the market snapshot and
// the exec id are refreshed, the latter because OKX's X-Perp ids carry a rolling expiry that
// genuinely changes under an unchanged symbol (CLAUDE.md §33.4).
func (r *Repository) UpsertInstrument(ctx context.Context, in port.Instrument) (port.Instrument, bool, error) {
	var out port.Instrument
	// xmax = 0 is the same "did THIS statement insert it" signal GetAccountEquity already uses
	// (internal/postgres/account_equity.go) — true only for a row this exact call created, false for
	// one the DO UPDATE branch merely touched.
	var inserted bool
	err := r.pool.QueryRow(ctx, `
		INSERT INTO instruments (symbol, exchange, exec_inst_id, inst_type,
			enabled_ingest, enabled_paper, enabled_real, source,
			vol_24h_usd, change_24h_pct, scan_score)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (exchange, symbol) DO UPDATE SET
			exec_inst_id   = EXCLUDED.exec_inst_id,
			inst_type      = EXCLUDED.inst_type,
			vol_24h_usd    = EXCLUDED.vol_24h_usd,
			change_24h_pct = EXCLUDED.change_24h_pct,
			scan_score     = EXCLUDED.scan_score,
			updated_at     = now()
		RETURNING `+instrumentCols+`, (xmax = 0)`,
		in.Symbol, in.Exchange, in.ExecInstID, in.InstType,
		in.EnabledIngest, in.EnabledPaper, in.EnabledReal, in.Source,
		in.Vol24hUSD, in.Change24hPct, in.ScanScore).
		Scan(&out.ID, &out.Symbol, &out.Exchange, &out.ExecInstID, &out.InstType,
			&out.EnabledIngest, &out.EnabledPaper, &out.EnabledReal, &out.Source,
			&out.Vol24hUSD, &out.Change24hPct, &out.ScanScore,
			&out.CreatedAt, &out.UpdatedAt, &inserted)
	if err != nil {
		return port.Instrument{}, false, fmt.Errorf("upsert instrument %s/%s: %w", in.Exchange, in.Symbol, err)
	}
	return out, inserted, nil
}

// SetInstrumentFlags applies patch's non-nil fields to one roster row. Each field is coalesced onto
// the existing value, so enabling a token for real money cannot clear its ingest flag as a side
// effect. Note the ::bool / ::text casts: a NULL parameter inside coalesce has no target column to
// infer a type from, which is the failure CLAUDE.md §36.3 records against a text[] column.
func (r *Repository) SetInstrumentFlags(ctx context.Context, id int64, patch port.InstrumentPatch) error {
	var ingest, paper, real, execID, instType any
	if patch.EnabledIngest != nil {
		ingest = *patch.EnabledIngest
	}
	if patch.EnabledPaper != nil {
		paper = *patch.EnabledPaper
	}
	if patch.EnabledReal != nil {
		real = *patch.EnabledReal
	}
	if patch.ExecInstID != nil {
		execID = *patch.ExecInstID
	}
	if patch.InstType != nil {
		instType = *patch.InstType
	}

	tag, err := r.pool.Exec(ctx, `
		UPDATE instruments SET
			enabled_ingest = coalesce($2::bool, enabled_ingest),
			enabled_paper  = coalesce($3::bool, enabled_paper),
			enabled_real   = coalesce($4::bool, enabled_real),
			exec_inst_id   = coalesce($5::text, exec_inst_id),
			inst_type      = coalesce($6::text, inst_type),
			updated_at     = now()
		WHERE id = $1`, id, ingest, paper, real, execID, instType)
	if err != nil {
		return fmt.Errorf("set instrument flags %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set instrument flags: no instrument with id %d", id)
	}
	return nil
}

// DeleteInstrument removes a roster row. Historical rows in candles/paper_orders keyed by its
// symbol are deliberately left alone — they are a record of what happened, not live state, the same
// call CLAUDE.md §33.5 made when symbols were renamed.
func (r *Repository) DeleteInstrument(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM instruments WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete instrument %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete instrument: no instrument with id %d", id)
	}
	return nil
}

// ReplaceMarketTokens overwrites one exchange's scanned market snapshot in a single transaction.
//
// Scoped per exchange and transactional for the same reason: one exchange's scan failing must
// neither wipe another exchange's good data nor leave this one half-written, which would show the
// panel a market that never existed. An empty toks slice is treated as "nothing to report" and
// leaves the previous snapshot in place rather than clearing it — an exchange returning no tickers
// is a fault, and deleting the last known-good view on a fault is the opposite of useful.
func (r *Repository) ReplaceMarketTokens(ctx context.Context, exchange string, toks []port.MarketToken) error {
	if len(toks) == 0 {
		return nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace market tokens: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM market_tokens WHERE exchange = $1`, exchange); err != nil {
		return fmt.Errorf("replace market tokens: clear %s: %w", exchange, err)
	}
	for _, t := range toks {
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_tokens (exchange, symbol, exec_inst_id, last_px, open_24h,
				high_24h, low_24h, vol_24h_usd, change_24h_pct, range_24h_pct, score, scanned_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now())`,
			exchange, t.Symbol, t.ExecInstID, t.LastPx, t.Open24h,
			t.High24h, t.Low24h, t.Vol24hUSD, t.Change24hPct, t.Range24hPct, t.Score); err != nil {
			return fmt.Errorf("replace market tokens: insert %s/%s: %w", exchange, t.Symbol, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace market tokens: commit: %w", err)
	}
	return nil
}

// ListMarketTokens returns the scanned market snapshot, highest score first. The panel re-sorts
// client-side by whichever column the operator clicks; this order is the default view and the one
// the "top N" limit is taken from, so it must be the composite score rather than any single column.
func (r *Repository) ListMarketTokens(ctx context.Context, exchange string, limit int) ([]port.MarketToken, error) {
	q := `SELECT exchange, symbol, exec_inst_id, last_px, open_24h, high_24h, low_24h,
		vol_24h_usd, change_24h_pct, range_24h_pct, score, scanned_at
		FROM market_tokens WHERE true`
	args := []any{}
	if exchange != "" {
		args = append(args, exchange)
		q += fmt.Sprintf(" AND exchange = $%d", len(args))
	}
	q += ` ORDER BY score DESC, symbol`
	if limit > 0 {
		args = append(args, limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list market tokens: %w", err)
	}
	defer rows.Close()

	var out []port.MarketToken
	for rows.Next() {
		var t port.MarketToken
		if err := rows.Scan(&t.Exchange, &t.Symbol, &t.ExecInstID, &t.LastPx, &t.Open24h,
			&t.High24h, &t.Low24h, &t.Vol24hUSD, &t.Change24hPct, &t.Range24hPct,
			&t.Score, &t.ScannedAt); err != nil {
			return nil, fmt.Errorf("scan market token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
