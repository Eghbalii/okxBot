package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

const accountEquityCols = `mode, initial_usd, equity_usd, account_balance_usd, trading_cap_usd, reset_count, last_reset_at, updated_at`

// accountEquityColsEx additionally selects exchange, for the exchange-scoped Ex methods below
// (2026-09-22, multi-exchange paper trading) — kept as a separate column list rather than adding
// exchange to accountEquityCols/scanAccountEquity so every pre-existing call site (bot/manual
// modes, which never pass an exchange) is untouched.
const accountEquityColsEx = accountEquityCols + `, exchange`

func scanAccountEquity(row interface {
	Scan(dest ...any) error
}, ae *port.AccountEquity) error {
	return row.Scan(&ae.Mode, &ae.InitialUSD, &ae.EquityUSD, &ae.AccountBalanceUSD, &ae.TradingCapUSD, &ae.ResetCount, &ae.LastResetAt, &ae.UpdatedAt)
}

func scanAccountEquityEx(row interface {
	Scan(dest ...any) error
}, ae *port.AccountEquity) error {
	return row.Scan(&ae.Mode, &ae.InitialUSD, &ae.EquityUSD, &ae.AccountBalanceUSD, &ae.TradingCapUSD, &ae.ResetCount, &ae.LastResetAt, &ae.UpdatedAt, &ae.Exchange)
}

// GetAccountEquity returns mode's current balance row, seeding it (plus a "seed" history point)
// if it doesn't exist yet — CLAUDE.md §15.6.
//
// For "paper" (and any future non-real-money mode), EquityUSD and AccountBalanceUSD both seed at
// initialUSD, unchanged from the original design: paper has no real exchange balance to divide, so
// there's nothing to hold back.
//
// For "bot"/"manual" (isRealMoneyMode), AccountBalanceUSD still seeds at initialUSD — the real
// balance genuinely exists the moment either mode first reads it — but EquityUSD seeds at ZERO
// (2026-09-20 fix; see RecordExchangeBalance's identical reasoning). The two real-money modes
// share one exchange balance and start with NEITHER claiming any of it: an operator who never
// explicitly sets a cap gets a mode that can open nothing, not one that silently owns the whole
// balance by default. The previous default (EquityUSD = initialUSD, i.e. "claims everything until
// told otherwise") is what let an uncapped mode's sibling-bound check in SetTradingCap clamp the
// OTHER mode's very first cap down to a few cents — the sibling still "claimed" the full balance at
// that moment, so almost nothing was left. Seeding at zero means an unconfigured mode claims
// nothing, so the sibling bound has real room until the operator actually allocates some.
func (r *Repository) GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	var ae port.AccountEquity
	seedEquity := initialUSD
	if isRealMoneyMode(mode) {
		seedEquity = decimal.Zero
	}
	// xmax = 0 identifies a row this statement actually inserted, as opposed to one the no-op
	// DO UPDATE just returned — that's what tells us whether to write the seed history point,
	// without a second round trip to check for existence first.
	// exchange is pinned to 'okx' explicitly — account_equity's key widened to (mode, exchange)
	// when multi-exchange paper trading landed (2026-09-22, migration 000037), so ON CONFLICT
	// (mode) alone no longer matches any constraint on this table. Every caller of this un-scoped
	// method (bot/manual/paper's own operator-facing endpoints) predates exchange scoping and must
	// keep reading/writing exactly the row it always did.
	var inserted bool
	row := r.pool.QueryRow(ctx, `
		INSERT INTO account_equity (mode, exchange, initial_usd, equity_usd, account_balance_usd)
		VALUES ($1, 'okx', $2, $3, $2)
		ON CONFLICT (mode, exchange) DO UPDATE SET mode = account_equity.mode
		RETURNING `+accountEquityCols+`, (xmax = 0)
	`, mode, initialUSD, seedEquity)
	if err := row.Scan(&ae.Mode, &ae.InitialUSD, &ae.EquityUSD, &ae.AccountBalanceUSD, &ae.TradingCapUSD, &ae.ResetCount, &ae.LastResetAt, &ae.UpdatedAt, &inserted); err != nil {
		return port.AccountEquity{}, fmt.Errorf("get account equity for mode %s: %w", mode, err)
	}

	if inserted {
		// Best-effort: the balance row itself is authoritative, so failing to write the opening
		// point of the chart must not fail the caller's read.
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO account_equity_history (mode, equity_usd, delta_usd, reason)
			VALUES ($1, $2, 0, 'seed')
		`, mode, ae.EquityUSD); err != nil {
			return ae, nil
		}
	}
	return ae, nil
}

// ApplyRealizedPnL adds pnl to mode's running balance, recording an EquityPoint for it, and resets
// a drained EquityUSD (never AccountBalanceUSD — see its doc comment) back to InitialUSD unless
// mode is "bot" — CLAUDE.md §15.6/§15.7. Real money never auto-tops-up: running out is a stop
// condition for a human, not a bookkeeping event.
//
// AccountBalanceUSD moves by the exact same pnl, in the exact same transaction, but is never reset
// by anything in this function — it is the real, continuous running total (CLAUDE.md §31.2),
// independent of whichever baseline EquityUSD is currently reset to.
//
// Runs in one transaction so a balance change and its history point can't diverge: a chart built
// from history rows that's missing the very drop that drained the account would be actively
// misleading, which is the failure this whole timeline exists to prevent.
func (r *Repository) ApplyRealizedPnL(
	ctx context.Context,
	mode string,
	pnl decimal.Decimal,
	orderID *int64,
	instID string,
) (port.AccountEquity, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("begin apply realized pnl: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	// exchange pinned to 'okx' explicitly throughout this method, for the same reason as
	// GetAccountEquity above: this un-scoped path predates exchange scoping and must keep
	// touching exactly the row it always did, not every exchange sharing this mode.
	var ae port.AccountEquity
	row := tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = equity_usd + $2,
			account_balance_usd = account_balance_usd + $2,
			updated_at = now()
		WHERE mode = $1 AND exchange = 'okx'
		RETURNING `+accountEquityCols, mode, pnl)
	if err := scanAccountEquity(row, &ae); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("apply realized pnl for mode %s: %w", mode, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason, order_id, inst_id)
		VALUES ($1, 'okx', $2, $3, 'trade', $4, $5)
	`, mode, ae.EquityUSD, pnl, orderID, nullableText(instID)); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("record equity history for mode %s: %w", mode, err)
	}

	// Real-money mode is deliberately excluded from the reset: see §15.7's carve-out.
	if ae.EquityUSD.Sign() > 0 || mode == "bot" {
		if err := tx.Commit(ctx); err != nil {
			return port.AccountEquity{}, false, fmt.Errorf("commit apply realized pnl: %w", err)
		}
		return ae, false, nil
	}

	drained := ae.EquityUSD
	row = tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = initial_usd,
			reset_count = reset_count + 1,
			last_reset_at = now(),
			updated_at = now()
		WHERE mode = $1 AND exchange = 'okx'
		RETURNING `+accountEquityCols, mode)
	if err := scanAccountEquity(row, &ae); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("reset drained account for mode %s: %w", mode, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, equity_usd, delta_usd, reason, inst_id)
		VALUES ($1, $2, $3, 'reset', $4)
	`, mode, ae.EquityUSD, ae.EquityUSD.Sub(drained), nullableText(instID)); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("record equity reset for mode %s: %w", mode, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("commit account reset: %w", err)
	}
	return ae, true, nil
}

// RecordExchangeBalance reconciles the exchange's own raw reported balance against this mode's
// stored AccountBalanceUSD, applying the real delta as a reason="trade" EquityPoint (mirroring
// ApplyRealizedPnL exactly), then derives EquityUSD independently as
// max(AccountBalanceUSD-safeMoneyUSD, 0) with NO history point of its own — CLAUDE.md §32's
// incident. Before this existed, BotTrader.recordEquityBot computed
// tradableEquity(rawBalance) = rawBalance-SafeMoneyUSD FIRST and fed that already-reserve-
// subtracted number into ApplyRealizedPnL's delta-from-EquityUSD comparison — the moment
// SafeMoneyUSD was set to a nonzero value, that read as a real trade loss equal to the reserve
// and dragged the real AccountBalanceUSD down by it too, even though nothing had actually
// happened to the exchange balance. This method takes the RAW balance instead and computes the
// delta against AccountBalanceUSD (the true running total), so a reserve being subtracted for
// display/sizing purposes can never be mistaken for a realized trade.
func (r *Repository) RecordExchangeBalance(ctx context.Context, mode string, rawBalanceUSD, safeMoneyUSD decimal.Decimal, instID string) (port.AccountEquity, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, fmt.Errorf("begin record exchange balance: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	var previousBalance, previousEquity decimal.Decimal
	var previousCap *decimal.Decimal
	if err := tx.QueryRow(ctx, `
		SELECT account_balance_usd, equity_usd, trading_cap_usd FROM account_equity WHERE mode = $1 AND exchange = 'okx'
	`, mode).Scan(&previousBalance, &previousEquity, &previousCap); err != nil {
		return port.AccountEquity{}, fmt.Errorf("record exchange balance for mode %s: no existing row (call GetAccountEquity first): %w", mode, err)
	}

	delta := rawBalanceUSD.Sub(previousBalance)

	// How tradable equity is derived depends on whether an explicit trading cap is set.
	//
	// With a cap (the 2026-09-08 model): the cap names the tradable slice at the moment it was
	// chosen, and realized PnL accrues to that slice — so equity moves by the SAME delta the real
	// balance just moved by, leaving the untraded reserve (balance - equity) constant. That is the
	// requested behavior exactly: $20 traded out of a $40 balance, then +$5, gives $25 tradable and
	// $45 total, with the reserve still $20. Deriving equity as "the cap" flat instead would pin
	// tradable equity at $20 forever and quietly discard every gain; deriving it as
	// "balance - reserve" is the same arithmetic stated in terms that survive the balance moving.
	//
	// Without a cap: a real-money mode (bot/manual) that has never been given a cap claims NOTHING
	// of the shared balance (2026-09-20 fix) — it must not default to claiming the whole thing, or
	// the very first cap set on its sibling would see this mode's uncapped equity as already
	// claiming 100% of the balance and clamp the sibling's cap down to almost zero (the exact bug
	// this fixes: setting "manual" to $20 while "bot" had never had a cap computed a sibling-bound
	// of balance-minus-bot's-full-mirrored-equity, leaving manual capped at a few cents no matter
	// what was requested). "paper" keeps the original SafeMoneyUSD-reserve fallback, since it has
	// no sibling and no real balance being divided — there's nothing to protect it from.
	var tradable decimal.Decimal
	switch {
	case previousCap != nil:
		reserve := previousBalance.Sub(previousEquity)
		tradable = rawBalanceUSD.Sub(reserve)
	case isRealMoneyMode(mode):
		tradable = decimal.Zero
	default:
		tradable = rawBalanceUSD.Sub(safeMoneyUSD)
	}
	if tradable.IsNegative() {
		tradable = decimal.Zero
	}

	var ae port.AccountEquity
	row := tx.QueryRow(ctx, `
		UPDATE account_equity
		SET account_balance_usd = $2,
			equity_usd = $3,
			updated_at = now()
		WHERE mode = $1 AND exchange = 'okx'
		RETURNING `+accountEquityCols, mode, rawBalanceUSD, tradable)
	if err := scanAccountEquity(row, &ae); err != nil {
		return port.AccountEquity{}, fmt.Errorf("record exchange balance for mode %s: %w", mode, err)
	}

	if !delta.IsZero() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason, inst_id)
			VALUES ($1, 'okx', $2, $3, 'trade', $4)
		`, mode, ae.EquityUSD, delta, nullableText(instID)); err != nil {
			return port.AccountEquity{}, fmt.Errorf("record exchange balance history for mode %s: %w", mode, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, fmt.Errorf("commit record exchange balance: %w", err)
	}
	return ae, nil
}

// SetAccountCap is a operator-triggered DEPOSIT/WITHDRAWAL, not an independent re-baselining of
// EquityUSD alone (corrected 2026-09-04, CLAUDE.md §31.3 — the first version of this method set
// EquityUSD to newCapUSD while leaving AccountBalanceUSD untouched, which left Balance sitting
// below Equity with a gap that had nothing to do with any real trade — mathematically incoherent,
// since neither field in this schema ever carries unrealized PnL: both only move on a position's
// close (ApplyRealizedPnL), so with no open positions Balance and Equity must be numerically
// identical at all times, exactly like a real exchange account with no unrealized PnL component).
//
// The correct model: choosing a new cap is economically a deposit or withdrawal bringing the real
// balance to newCapUSD, so BOTH InitialUSD/EquityUSD ("Total Equity" — resets to the chosen
// baseline) AND AccountBalanceUSD ("Account Balance" — CLAUDE.md §31.2's real continuous total)
// are set to newCapUSD together, by the SAME operation, in the SAME transaction. This is the only
// formula under which Balance >= Equity holds as an invariant rather than as a coincidence: after
// this call they are equal, and they can only diverge again through PnL that touches both fields
// identically (ApplyRealizedPnL), which by construction cannot separate them either.
func (r *Repository) SetAccountCap(ctx context.Context, mode string, newCapUSD decimal.Decimal) (port.AccountEquity, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, fmt.Errorf("begin set account cap: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	// The previous EquityUSD, so delta_usd on the history row is the true top-up/draw-down amount
	// (newCapUSD - previous), matching ApplyRealizedPnL's own reset convention — not just
	// newCapUSD itself, which would misreport the change whenever the account wasn't already at
	// zero when the operator chose a new cap. A missing row (fresh mode) has no "previous", so the
	// delta is simply the new cap, same as GetAccountEquity's own first-seed behavior.
	var previous decimal.Decimal
	if err := tx.QueryRow(ctx, `SELECT equity_usd FROM account_equity WHERE mode = $1 AND exchange = 'okx'`, mode).Scan(&previous); err != nil {
		previous = decimal.Zero
	}

	var ae port.AccountEquity
	row := tx.QueryRow(ctx, `
		INSERT INTO account_equity (mode, exchange, initial_usd, equity_usd, account_balance_usd, reset_count, last_reset_at)
		VALUES ($1, 'okx', $2, $2, $2, 1, now())
		ON CONFLICT (mode, exchange) DO UPDATE SET
			initial_usd = $2,
			equity_usd = $2,
			account_balance_usd = $2,
			reset_count = account_equity.reset_count + 1,
			last_reset_at = now(),
			updated_at = now()
		RETURNING `+accountEquityCols, mode, newCapUSD)
	if err := scanAccountEquity(row, &ae); err != nil {
		return port.AccountEquity{}, fmt.Errorf("set account cap for mode %s: %w", mode, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason)
		VALUES ($1, 'okx', $2, $3, 'reset')
	`, mode, newCapUSD, newCapUSD.Sub(previous)); err != nil {
		return port.AccountEquity{}, fmt.Errorf("record account cap reset for mode %s: %w", mode, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, fmt.Errorf("commit set account cap: %w", err)
	}
	return ae, nil
}

// AdjustAccountCap ADDS deltaUSD to both EquityUSD and AccountBalanceUSD — see the port interface's
// doc comment for why this is a separate operation from SetAccountCap rather than a mode of it.
//
// Requires an existing row (same precondition as SetTradingCap): a per-token top-up only makes
// sense once GetAccountEquity has already seeded the mode, and seeding here too would let a caller
// that forgot to seed first silently create the account at whatever the first delta happened to be.
func (r *Repository) AdjustAccountCap(ctx context.Context, mode string, deltaUSD decimal.Decimal) (port.AccountEquity, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, fmt.Errorf("begin adjust account cap: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	var ae port.AccountEquity
	row := tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = equity_usd + $2,
			account_balance_usd = account_balance_usd + $2,
			updated_at = now()
		WHERE mode = $1 AND exchange = 'okx'
		RETURNING `+accountEquityCols, mode, deltaUSD)
	if err := scanAccountEquity(row, &ae); err != nil {
		return port.AccountEquity{}, fmt.Errorf("adjust account cap for mode %s: no existing row (call GetAccountEquity first): %w", mode, err)
	}

	// reason="cap": this is sizing-budget bookkeeping following the roster's own growth, not a
	// trading outcome — it must never reach the PnL/win-rate figures a reason="trade" row would.
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason)
		VALUES ($1, 'okx', $2, $3, 'cap')
	`, mode, ae.EquityUSD, deltaUSD); err != nil {
		return port.AccountEquity{}, fmt.Errorf("record account cap adjustment for mode %s: %w", mode, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, fmt.Errorf("commit adjust account cap: %w", err)
	}
	return ae, nil
}

// AdjustAccountCapEx is the exchange-scoped sibling of AdjustAccountCap above (2026-09-22,
// multi-exchange paper trading) — used by MarketScanner's per-token top-up so a second
// paper-trading profile's (e.g. "mexc") own account row grows with ITS OWN newly-admitted
// tokens, not OKX's. Same precondition as AdjustAccountCap: requires an existing (mode, exchange)
// row (GetAccountEquityEx must have seeded it first). exchange="" behaves exactly like
// AdjustAccountCap (defaults to "okx").
func (r *Repository) AdjustAccountCapEx(ctx context.Context, mode, exchange string, deltaUSD decimal.Decimal) (port.AccountEquity, error) {
	if exchange == "" {
		exchange = "okx"
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, fmt.Errorf("begin adjust account cap: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	var ae port.AccountEquity
	row := tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = equity_usd + $3,
			account_balance_usd = account_balance_usd + $3,
			updated_at = now()
		WHERE mode = $1 AND exchange = $2
		RETURNING `+accountEquityColsEx, mode, exchange, deltaUSD)
	if err := scanAccountEquityEx(row, &ae); err != nil {
		return port.AccountEquity{}, fmt.Errorf("adjust account cap for mode %s, exchange %s: no existing row (call GetAccountEquityEx first): %w", mode, exchange, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason)
		VALUES ($1, $2, $3, $4, 'cap')
	`, mode, exchange, ae.EquityUSD, deltaUSD); err != nil {
		return port.AccountEquity{}, fmt.Errorf("record account cap adjustment for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, fmt.Errorf("commit adjust account cap: %w", err)
	}
	return ae, nil
}

// realMoneyModes are every mode that shares ONE real exchange balance (Account page, 2026-09-20
// request: bot trading and manual trading are two separate slices of the same OKX account, not
// two independent pools) — used by SetTradingCap to bound one mode's cap against what the OTHER
// real mode has already claimed, so their caps can never jointly exceed the real balance. "paper"
// is deliberately excluded: it has its own fictional balance with nothing to reconcile against.
var realMoneyModes = []string{"bot", "manual"}

// isRealMoneyMode reports whether mode is one of the slices sharing the one real exchange balance
// (as opposed to "paper", which has no real balance to divide at all).
func isRealMoneyMode(mode string) bool {
	for _, m := range realMoneyModes {
		if m == mode {
			return true
		}
	}
	return false
}

// SetTradingCap sets how much of the REAL balance this engine may trade with, without ever
// touching AccountBalanceUSD — see the port interface's doc comment for why that separation is
// mandatory in real mode (AccountBalanceUSD is RecordExchangeBalance's reconciliation anchor
// against the exchange's own reported number; overwriting it makes the next poll report the
// difference as realized PnL that never happened).
//
// EquityUSD becomes the cap, bounded above by (real balance - whatever the OTHER real-money mode
// has already claimed) — a cap larger than what's actually left cannot be honored. Two independent
// per-mode bounds against the full balance alone would let bot and manual each believe they own the
// whole account: a $40 balance with bot capped at $30 must leave manual capped at $10, not another
// $30, even though $30 alone is <= the $40 balance either mode would check against on its own
// (2026-09-20 request: bot and manual are two slices of ONE real account, not two accounts). The
// untraded remainder (balance - cap - sibling's cap) is the reserve, derived rather than stored, so
// it can never drift out of agreement with the numbers it sits between.
func (r *Repository) SetTradingCap(ctx context.Context, mode string, capUSD decimal.Decimal) (port.AccountEquity, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, fmt.Errorf("begin set trading cap: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	var previousEquity decimal.Decimal
	if err := tx.QueryRow(ctx, `SELECT equity_usd FROM account_equity WHERE mode = $1 AND exchange = 'okx'`, mode).Scan(&previousEquity); err != nil {
		return port.AccountEquity{}, fmt.Errorf("set trading cap for mode %s: no existing row (call GetAccountEquity first): %w", mode, err)
	}

	// The sibling real-money mode's currently-claimed slice (its own equity_usd, which IS its
	// current cap once one has been set — see SetTradingCap's own effect on equity_usd below), read
	// inside this same transaction so it can't be changed by a concurrent SetTradingCap call on the
	// sibling between this read and the UPDATE. A missing sibling row (never seeded, e.g. manual
	// trading has never been used) claims nothing.
	var siblingClaimed decimal.Decimal
	for _, sibling := range realMoneyModes {
		if sibling == mode {
			continue
		}
		var claimed decimal.Decimal
		if err := tx.QueryRow(ctx, `SELECT equity_usd FROM account_equity WHERE mode = $1 AND exchange = 'okx'`, sibling).Scan(&claimed); err == nil {
			siblingClaimed = siblingClaimed.Add(claimed)
		}
	}

	var ae port.AccountEquity
	// LEAST() applies both bounds — the real balance, and the balance minus whatever the sibling
	// mode already claimed — in SQL rather than in Go, so the stored cap and the derived equity are
	// decided by one expression against one snapshot of the balance: a read-then-write in Go could
	// interleave with a concurrent RecordExchangeBalance/sibling SetTradingCap and store a cap that
	// was valid against numbers no longer current. GREATEST(..., 0) floors the sibling-aware bound
	// at zero rather than letting it go negative when the sibling alone already claims the whole
	// balance, which LEAST() would otherwise happily propagate into a negative cap.
	row := tx.QueryRow(ctx, `
		UPDATE account_equity
		SET trading_cap_usd = $2,
			equity_usd = LEAST($2, account_balance_usd, GREATEST(account_balance_usd - $3, 0)),
			updated_at = now()
		WHERE mode = $1 AND exchange = 'okx'
		RETURNING `+accountEquityCols, mode, capUSD, siblingClaimed)
	if err := scanAccountEquity(row, &ae); err != nil {
		return port.AccountEquity{}, fmt.Errorf("set trading cap for mode %s: %w", mode, err)
	}

	// reason="cap", not "reset": this re-splits a balance that did not itself move, so it must not
	// stamp LastResetAt (which the chart's default window and "balance since I chose a baseline"
	// both anchor to) the way a genuine re-baselining does.
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason)
		VALUES ($1, 'okx', $2, $3, 'cap')
	`, mode, ae.EquityUSD, ae.EquityUSD.Sub(previousEquity)); err != nil {
		return port.AccountEquity{}, fmt.Errorf("record trading cap change for mode %s: %w", mode, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, fmt.Errorf("commit set trading cap: %w", err)
	}
	return ae, nil
}

// ListEquityHistory returns mode's balance timeline oldest-first, ready to plot directly. Pinned
// to exchange='okx' explicitly (this un-scoped method predates exchange scoping, 2026-09-22 —
// see ListEquityHistoryEx for the exchange-parameterized sibling PaperTrader uses).
func (r *Repository) ListEquityHistory(ctx context.Context, mode string, since time.Time, limit int) ([]port.EquityPoint, error) {
	// Ordered DESC with the LIMIT so a capped read returns the MOST RECENT points (a chart wants
	// the latest window, not the first N rows ever written), then reversed to oldest-first below.
	q := `
		SELECT id, mode, equity_usd, delta_usd, reason, order_id, COALESCE(inst_id, ''), created_at
		FROM account_equity_history
		WHERE mode = $1 AND exchange = 'okx' AND ($2::timestamptz IS NULL OR created_at >= $2)
		ORDER BY created_at DESC, id DESC`
	args := []any{mode, nullableTime(since)}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list equity history for mode %s: %w", mode, err)
	}
	defer rows.Close()

	var out []port.EquityPoint
	for rows.Next() {
		var p port.EquityPoint
		if err := rows.Scan(&p.ID, &p.Mode, &p.EquityUSD, &p.DeltaUSD, &p.Reason, &p.OrderID, &p.InstID, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan equity history row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate equity history: %w", err)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// GetAccountEquityEx/ApplyRealizedPnLEx/ListEquityHistoryEx are exchange-scoped siblings of
// GetAccountEquity/ApplyRealizedPnL/ListEquityHistory above (2026-09-22, multi-exchange paper
// trading) — used ONLY by usecase.PaperTrader, so a second, fully independent paper-trading
// instance against a different exchange keeps a completely separate balance/history under the
// same mode="paper", rather than sharing the single OKX instance's row.
//
// exchange="" defaults to "okx", so every un-scoped call elsewhere in this codebase is unaffected
// by this migration's DEFAULT 'okx' column — these Ex methods are additive, not a replacement.

// GetAccountEquityEx mirrors GetAccountEquity, keyed by (mode, exchange) instead of mode alone.
func (r *Repository) GetAccountEquityEx(ctx context.Context, mode, exchange string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	if exchange == "" {
		exchange = "okx"
	}
	var ae port.AccountEquity
	seedEquity := initialUSD
	if isRealMoneyMode(mode) {
		seedEquity = decimal.Zero
	}
	var inserted bool
	row := r.pool.QueryRow(ctx, `
		INSERT INTO account_equity (mode, exchange, initial_usd, equity_usd, account_balance_usd)
		VALUES ($1, $2, $3, $4, $3)
		ON CONFLICT (mode, exchange) DO UPDATE SET mode = account_equity.mode
		RETURNING `+accountEquityColsEx+`, (xmax = 0)
	`, mode, exchange, initialUSD, seedEquity)
	if err := row.Scan(&ae.Mode, &ae.InitialUSD, &ae.EquityUSD, &ae.AccountBalanceUSD, &ae.TradingCapUSD, &ae.ResetCount, &ae.LastResetAt, &ae.UpdatedAt, &ae.Exchange, &inserted); err != nil {
		return port.AccountEquity{}, fmt.Errorf("get account equity for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if inserted {
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason)
			VALUES ($1, $2, $3, 0, 'seed')
		`, mode, exchange, ae.EquityUSD); err != nil {
			return ae, nil
		}
	}
	return ae, nil
}

// ApplyRealizedPnLEx mirrors ApplyRealizedPnL, keyed by (mode, exchange) instead of mode alone.
func (r *Repository) ApplyRealizedPnLEx(
	ctx context.Context,
	mode, exchange string,
	pnl decimal.Decimal,
	orderID *int64,
	instID string,
) (port.AccountEquity, bool, error) {
	if exchange == "" {
		exchange = "okx"
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("begin apply realized pnl: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	var ae port.AccountEquity
	row := tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = equity_usd + $3,
			account_balance_usd = account_balance_usd + $3,
			updated_at = now()
		WHERE mode = $1 AND exchange = $2
		RETURNING `+accountEquityColsEx, mode, exchange, pnl)
	if err := scanAccountEquityEx(row, &ae); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("apply realized pnl for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason, order_id, inst_id)
		VALUES ($1, $2, $3, $4, 'trade', $5, $6)
	`, mode, exchange, ae.EquityUSD, pnl, orderID, nullableText(instID)); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("record equity history for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if ae.EquityUSD.Sign() > 0 || mode == "bot" {
		if err := tx.Commit(ctx); err != nil {
			return port.AccountEquity{}, false, fmt.Errorf("commit apply realized pnl: %w", err)
		}
		return ae, false, nil
	}

	drained := ae.EquityUSD
	row = tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = initial_usd,
			reset_count = reset_count + 1,
			last_reset_at = now(),
			updated_at = now()
		WHERE mode = $1 AND exchange = $2
		RETURNING `+accountEquityColsEx, mode, exchange)
	if err := scanAccountEquityEx(row, &ae); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("reset drained account for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason, inst_id)
		VALUES ($1, $2, $3, $4, 'reset', $5)
	`, mode, exchange, ae.EquityUSD, ae.EquityUSD.Sub(drained), nullableText(instID)); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("record equity reset for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("commit account reset: %w", err)
	}
	return ae, true, nil
}

// ListEquityHistoryEx mirrors ListEquityHistory, keyed by (mode, exchange) instead of mode alone.
func (r *Repository) ListEquityHistoryEx(ctx context.Context, mode, exchange string, since time.Time, limit int) ([]port.EquityPoint, error) {
	if exchange == "" {
		exchange = "okx"
	}
	q := `
		SELECT id, mode, equity_usd, delta_usd, reason, order_id, COALESCE(inst_id, ''), created_at
		FROM account_equity_history
		WHERE mode = $1 AND exchange = $2 AND ($3::timestamptz IS NULL OR created_at >= $3)
		ORDER BY created_at DESC, id DESC`
	args := []any{mode, exchange, nullableTime(since)}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list equity history for mode %s, exchange %s: %w", mode, exchange, err)
	}
	defer rows.Close()

	var out []port.EquityPoint
	for rows.Next() {
		var p port.EquityPoint
		if err := rows.Scan(&p.ID, &p.Mode, &p.EquityUSD, &p.DeltaUSD, &p.Reason, &p.OrderID, &p.InstID, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan equity history row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate equity history: %w", err)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// SetAccountCapEx mirrors SetAccountCap, keyed by (mode, exchange) instead of mode alone
// (2026-09-22, multi-exchange paper trading) — see that function's own doc comment for why Balance
// and Equity move together here.
func (r *Repository) SetAccountCapEx(ctx context.Context, mode, exchange string, newCapUSD decimal.Decimal) (port.AccountEquity, error) {
	if exchange == "" {
		exchange = "okx"
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, fmt.Errorf("begin set account cap: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	var previous decimal.Decimal
	if err := tx.QueryRow(ctx, `SELECT equity_usd FROM account_equity WHERE mode = $1 AND exchange = $2`, mode, exchange).Scan(&previous); err != nil {
		previous = decimal.Zero
	}

	var ae port.AccountEquity
	row := tx.QueryRow(ctx, `
		INSERT INTO account_equity (mode, exchange, initial_usd, equity_usd, account_balance_usd, reset_count, last_reset_at)
		VALUES ($1, $2, $3, $3, $3, 1, now())
		ON CONFLICT (mode, exchange) DO UPDATE SET
			initial_usd = $3,
			equity_usd = $3,
			account_balance_usd = $3,
			reset_count = account_equity.reset_count + 1,
			last_reset_at = now(),
			updated_at = now()
		RETURNING `+accountEquityColsEx, mode, exchange, newCapUSD)
	if err := scanAccountEquityEx(row, &ae); err != nil {
		return port.AccountEquity{}, fmt.Errorf("set account cap for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, exchange, equity_usd, delta_usd, reason)
		VALUES ($1, $2, $3, $4, 'reset')
	`, mode, exchange, newCapUSD, newCapUSD.Sub(previous)); err != nil {
		return port.AccountEquity{}, fmt.Errorf("record account cap reset for mode %s, exchange %s: %w", mode, exchange, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, fmt.Errorf("commit set account cap: %w", err)
	}
	return ae, nil
}

func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
