package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// OpenPaperOrder inserts a new virtual trade and returns its id.
func (r *Repository) OpenPaperOrder(ctx context.Context, o port.PaperOrder) (int64, error) {
	mode := o.Mode
	if mode == "" {
		mode = "paper"
	}
	variant := o.Variant
	if variant == "" {
		variant = "baseline"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_orders (inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, features_json, mode, parent_order_id, variant, bar, exchange_order_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id
	`, o.InstID, o.StrategyID, o.Side, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.FeaturesJSON, mode, o.ParentOrderID, variant, o.Bar, o.ExchangeOrderID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open paper order for %s: %w", o.InstID, err)
	}
	return id, nil
}

// GetPaperOrder fetches a single order by id, open or closed (CLAUDE.md §27.3's plan §3b — the
// manual SL/TP-edit endpoint needs EntryPx/Leverage/Side for one specific order, a shape none of
// the existing list-oriented reads provide directly).
func (r *Repository) GetPaperOrder(ctx context.Context, id int64) (port.PaperOrder, error) {
	var o port.PaperOrder
	var bar *string
	err := r.pool.QueryRow(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at,
			closed_at, close_reason, close_px, realized_pnl, features_json, mode, parent_order_id,
			variant, bar, pnl_max_pct, pnl_min_pct, manual_close_requested, exchange_order_id, exchange_algo_order_id
		FROM paper_orders WHERE id = $1
	`, id).Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size,
		&o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx, &o.RealizedPnL,
		&o.FeaturesJSON, &o.Mode, &o.ParentOrderID, &o.Variant, &bar, &o.PnLMaxPct, &o.PnLMinPct,
		&o.ManualCloseRequested, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID)
	if err != nil {
		return port.PaperOrder{}, fmt.Errorf("get paper order %d: %w", id, err)
	}
	if bar != nil {
		o.Bar = *bar
	}
	return o, nil
}

// SetExchangeAlgoOrderID records the resting SL/TP algo order's OKX-assigned ID on an already-open
// real order (CLAUDE.md §27.3) — a separate call from OpenPaperOrder because the algo order isn't
// placed until after the entry order's row already exists.
func (r *Repository) SetExchangeAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE paper_orders SET exchange_algo_order_id = $2 WHERE id = $1
	`, id, algoOrderID)
	if err != nil {
		return fmt.Errorf("set exchange algo order id for paper order %d: %w", id, err)
	}
	return nil
}

// ClosePaperOrder marks a virtual trade closed with its realized outcome.
func (r *Repository) ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL, feesUSD, fundingUSD decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE paper_orders
		SET closed_at = now(), close_px = $2, close_reason = $3, realized_pnl = $4, fees_usd = $5, funding_usd = $6
		WHERE id = $1
	`, id, closePx, reason, realizedPnL, feesUSD, fundingUSD)
	if err != nil {
		return fmt.Errorf("close paper order %d: %w", id, err)
	}
	return nil
}

// UpdatePaperOrderSLTP applies an in-trade SL/TP adjustment to an open order (CLAUDE.md §15.4).
// The caller must have already run the proposed prices through the ratchet clamp
// (usecase.RatchetSLTP) — this only persists whatever it's given, scoped to still-open orders so a
// stale/late adjustment can never resurrect a since-closed order's SL/TP.
func (r *Repository) UpdatePaperOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE paper_orders
		SET sl_px = $2, tp_px = $3
		WHERE id = $1 AND closed_at IS NULL
	`, id, slPx, tpPx)
	if err != nil {
		return fmt.Errorf("update paper order %d SL/TP: %w", id, err)
	}
	return nil
}

// ListOpenPaperOrders returns still-open virtual trades for an instrument (both baseline and
// rl_adjusted-fork variants — the SL/TP monitor loop treats them uniformly, each hits its own
// SL/TP independently; only reward/budget attribution filters by Variant, CLAUDE.md §15.4).
func (r *Repository) ListOpenPaperOrders(ctx context.Context, instID string) ([]port.PaperOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at, features_json, parent_order_id, variant, pnl_max_pct, pnl_min_pct, bar, manual_close_requested, exchange_order_id, exchange_algo_order_id
		FROM paper_orders
		WHERE inst_id = $1 AND closed_at IS NULL
		ORDER BY opened_at
	`, instID)
	if err != nil {
		return nil, fmt.Errorf("list open paper orders for %s: %w", instID, err)
	}
	defer rows.Close()

	var out []port.PaperOrder
	for rows.Next() {
		var o port.PaperOrder
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.OpenedAt, &o.FeaturesJSON, &o.ParentOrderID, &o.Variant, &o.PnLMaxPct, &o.PnLMinPct, &bar, &o.ManualCloseRequested, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID); err != nil {
			return nil, fmt.Errorf("scan paper order: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RequestManualClose flags an open order for PaperTrader to close on its next tick (CLAUDE.md
// 2026-08-31 panel Close button). Errors if id is not currently open — closing an already-closed
// or nonexistent order silently would hide a stale/duplicate request from the panel.
func (r *Repository) RequestManualClose(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE paper_orders SET manual_close_requested = true
		WHERE id = $1 AND closed_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("request manual close for order %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("order %d is not open", id)
	}
	return nil
}

// positionSortColumns maps PositionFilter.SortBy to a safe, allowlisted SQL column/expression —
// never interpolate the caller-provided SortBy string directly into the query. Default changed to
// closed_at (2026-09-02): a closed-position query is what actually got slow as the table grew,
// and the panel's own default view for that filter is "most recently closed first" — opened_at
// was never the interesting axis there.
var positionSortColumns = map[string]string{
	"":          "closed_at",
	"opened_at": "opened_at",
	"closed_at": "closed_at",
	"pnl":       "realized_pnl",
	"inst_id":   "inst_id",
}

// ListPositions returns positions across trading modes for the panel (CLAUDE.md §11.4), filtered,
// sorted, and paged per f. Backs paper trading today; demo/real rows land in the same table (mode
// column) once cmd/trader writes them, so this query needs no changes when that lands.
//
// f.Limit/Offset (added 2026-09-02): with closed positions numbering in the hundreds, the panel's
// old unbounded fetch-everything-then-paginate-client-side approach became a genuinely slow query
// and a multi-MB payload (FeaturesJSON alone averages ~3.8KB/row) on every 5s poll. Limit<=0 keeps
// the old unbounded behavior for callers that need the full set (e.g. an open-positions-only scan,
// which is a small row count regardless).
func (r *Repository) ListPositions(ctx context.Context, f port.PositionFilter) ([]port.PaperOrder, error) {
	col, ok := positionSortColumns[f.SortBy]
	if !ok {
		return nil, fmt.Errorf("list positions: unknown sort field %q", f.SortBy)
	}
	dir := "ASC"
	if f.SortDesc {
		dir = "DESC"
	}
	// NULLS LAST keeps still-open positions (closed_at/realized_pnl NULL) from jumping to the top
	// of a DESC sort, which would otherwise bury every closed position the panel wants to compare.
	orderClause := fmt.Sprintf("%s %s NULLS LAST", col, dir)

	// LEFT JOIN, not JOIN: an order with no strategy attribution (strategy_id NULL) or one whose
	// strategy row was later deleted must still show up in the positions list — it just renders
	// with an empty strategy name rather than disappearing from the panel entirely.
	query := `
		SELECT po.id, po.inst_id, po.strategy_id, po.side, po.entry_px, po.sl_px, po.tp_px, po.size, po.leverage,
			po.opened_at, po.closed_at, po.close_reason, po.close_px, po.realized_pnl, po.fees_usd, po.funding_usd, po.features_json, po.mode,
			po.parent_order_id, po.variant, po.bar, po.pnl_max_pct, po.pnl_min_pct, COALESCE(s.name, ''),
			po.exchange_order_id, po.exchange_algo_order_id,
			-- How many in-place SL/TP edits this order has had. The panel's "Updated" column used to
			-- be derived from parent_order_id (was this order shadow-forked?), but forking was
			-- replaced by in-place edits on 2026-09-02 (CLAUDE.md §15.4 revision), so that column has
			-- been permanently blank ever since: zero forks exist, while the adjustment log holds
			-- hundreds of real edits. Counted here rather than joined so an order with no
			-- adjustments still returns exactly one row.
			(SELECT COUNT(*) FROM paper_order_adjustments a WHERE a.order_id = po.id)
		FROM paper_orders po
		LEFT JOIN strategies s ON s.id = po.strategy_id
		WHERE ($1 = '' OR po.mode = $1)
			AND ($2 = '' OR po.inst_id = $2)
			AND ($3::boolean IS NULL OR (po.closed_at IS NULL) = $3)
		ORDER BY ` + orderClause

	args := []any{f.Mode, f.InstID, f.Open}
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		args = append(args, f.Limit, f.Offset)
	}

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list positions: %w", err)
	}
	defer rows.Close()

	var out []port.PaperOrder
	for rows.Next() {
		var o port.PaperOrder
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx,
			&o.Size, &o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx,
			&o.RealizedPnL, &o.FeesUSD, &o.FundingUSD, &o.FeaturesJSON, &o.Mode, &o.ParentOrderID, &o.Variant, &bar,
			&o.PnLMaxPct, &o.PnLMinPct, &o.StrategyName, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID,
			&o.AdjustmentCount); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CountPositions returns how many rows f's Mode/InstID/Open filters match — what the panel's
// pagination control needs to compute total page count without pulling every row back.
func (r *Repository) CountPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM paper_orders po
		WHERE ($1 = '' OR po.mode = $1)
			AND ($2 = '' OR po.inst_id = $2)
			AND ($3::boolean IS NULL OR (po.closed_at IS NULL) = $3)
	`, f.Mode, f.InstID, f.Open).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count positions: %w", err)
	}
	return count, nil
}

// TokenStats24h computes each token's last-24h activity for mode ("paper" or "real") — CLAUDE.md,
// "Manage tokens" panel, 2026-09-04 request, extended to real trading by the real-trading
// readiness plan (2026-09-04) — position count and PnL$/PnL% for trades CLOSED in the last 24
// hours, sourced from paper_orders (filtered to variant='baseline', excluding shadow forks —
// CLAUDE.md §16.9) or real_orders (no variant column, every row counts) depending on mode. PnL% is
// expressed against the token's own summed entry notional in the window (return on capital
// deployed for that token), not the shared account's equity — a token has no "starting equity" of
// its own the way the whole account does (CLAUDE.md §15.6's shared pool).
func (r *Repository) TokenStats24h(ctx context.Context, mode string) ([]port.TokenStats, error) {
	var query string
	if mode == "real" {
		query = `
			SELECT inst_id, count(*), coalesce(sum(realized_pnl), 0), coalesce(sum(size), 0)
			FROM real_orders
			WHERE closed_at IS NOT NULL AND closed_at >= now() - interval '24 hours'
			GROUP BY inst_id
		`
	} else {
		query = `
			SELECT inst_id, count(*), coalesce(sum(realized_pnl), 0), coalesce(sum(size), 0)
			FROM paper_orders
			WHERE mode = 'paper' AND variant = 'baseline'
				AND closed_at IS NOT NULL AND closed_at >= now() - interval '24 hours'
			GROUP BY inst_id
		`
	}
	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("token stats 24h (mode %s): %w", mode, err)
	}
	defer rows.Close()

	var out []port.TokenStats
	for rows.Next() {
		var s port.TokenStats
		var notionalSum decimal.Decimal
		if err := rows.Scan(&s.InstID, &s.PositionCount, &s.PnLUSD, &notionalSum); err != nil {
			return nil, fmt.Errorf("token stats 24h scan: %w", err)
		}
		if notionalSum.IsPositive() {
			s.PnLPct = s.PnLUSD.Div(notionalSum).Mul(decimal.NewFromInt(100))
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("token stats 24h rows: %w", err)
	}
	return out, nil
}

// UpdatePaperOrderPnLExtremes advances an open order's peak/trough unrealized PnL (CLAUDE.md
// §15.11). GREATEST/LEAST are applied in SQL rather than in Go so a concurrent writer can never
// walk a high-water mark backwards — the tick handler and any other caller may both be updating
// the same order, and a read-modify-write in Go would lose whichever update landed first.
func (r *Repository) UpdatePaperOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE paper_orders
		SET pnl_max_pct = GREATEST(pnl_max_pct, $2),
			pnl_min_pct = LEAST(pnl_min_pct, $3)
		WHERE id = $1 AND closed_at IS NULL
	`, id, maxPct, minPct)
	if err != nil {
		return fmt.Errorf("update pnl extremes for paper order %d: %w", id, err)
	}
	return nil
}
