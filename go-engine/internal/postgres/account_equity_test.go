package postgres

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// SetTradingCap must bound one real-money mode's cap against what the OTHER real-money mode has
// already claimed, not just against the raw exchange balance — bot and manual share ONE real
// account (Account page, 2026-09-20 request), so a $40 balance with bot capped at $30 must leave
// manual capped at $10, never another $30. Run against a REAL database, matching this package's own
// established practice (paper_trading_config_test.go's doc comment: a fake repository cannot
// reproduce Postgres's own SQL/type behavior, which is exactly what this LEAST/GREATEST expression
// needs verified for real).
func TestSetTradingCap_BoundsAgainstSiblingModesClaim(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	// Snapshot both real-money modes' current rows so this test can restore them afterward —
	// this suite runs against whatever database POSTGRES_DSN points at, which may be a real
	// deployment's own bot/manual accounts.
	botBefore, err := repo.GetAccountEquity(ctx, "bot", decimal.NewFromInt(100))
	if err != nil {
		t.Fatalf("read bot account: %v", err)
	}
	manualBefore, err := repo.GetAccountEquity(ctx, "manual", decimal.NewFromInt(100))
	if err != nil {
		t.Fatalf("read manual account: %v", err)
	}
	// Restored via direct SQL, not SetTradingCap: the public API has no way to express "no cap at
	// all" (SetTradingCap always sets one), so going through it here would leave an originally-
	// uncapped row with a cap this test invented — corrupting the NEXT test's own "before" state
	// exactly the way TestSetTradingCap_SiblingReleasingClaimFreesRoomOnNextCall was found to fail
	// when run after this one (both real bugs this test's cleanup was masking, not introducing).
	restore := func(mode string, before port.AccountEquity) {
		if _, err := repo.pool.Exec(ctx, `
			UPDATE account_equity SET equity_usd = $2, account_balance_usd = $3, trading_cap_usd = $4 WHERE mode = $1
		`, mode, before.EquityUSD, before.AccountBalanceUSD, before.TradingCapUSD); err != nil {
			t.Logf("cleanup: failed to restore %s account_equity row: %v", mode, err)
		}
	}
	t.Cleanup(func() {
		restore("bot", botBefore)
		restore("manual", manualBefore)
	})

	// Establish a known real balance on both rows via RecordExchangeBalance, the same way a real
	// reconciliation poll would. Neither mode has a cap yet, so EquityUSD stays at ZERO on both
	// (2026-09-20 fix) even though AccountBalanceUSD is now 40 — an uncapped real-money mode claims
	// nothing until the operator explicitly sets a cap, which is exactly the property this test's
	// own sibling-bound scenario below depends on: SetTradingCap("bot", 30) must succeed with the
	// FULL $30 (not clamped), because "manual" hasn't claimed any of the $40 yet.
	balance := decimal.NewFromInt(40)
	botSeeded, err := repo.RecordExchangeBalance(ctx, "bot", balance, decimal.Zero, "")
	if err != nil {
		t.Fatalf("seed bot balance: %v", err)
	}
	if !botSeeded.EquityUSD.IsZero() {
		t.Fatalf("bot equity after seeding with no cap = %s, want 0 (an uncapped real-money mode claims nothing)", botSeeded.EquityUSD)
	}
	manualSeeded, err := repo.RecordExchangeBalance(ctx, "manual", balance, decimal.Zero, "")
	if err != nil {
		t.Fatalf("seed manual balance: %v", err)
	}
	if !manualSeeded.EquityUSD.IsZero() {
		t.Fatalf("manual equity after seeding with no cap = %s, want 0 (an uncapped real-money mode claims nothing)", manualSeeded.EquityUSD)
	}

	// Bot claims $30 of the shared $40 balance.
	botAE, err := repo.SetTradingCap(ctx, "bot", decimal.NewFromInt(30))
	if err != nil {
		t.Fatalf("set bot cap: %v", err)
	}
	if !botAE.EquityUSD.Equal(decimal.NewFromInt(30)) {
		t.Fatalf("bot equity = %s, want 30 (nothing claimed by manual yet)", botAE.EquityUSD)
	}

	// Manual asks for $30 too — but only $10 is actually left ($40 balance - $30 bot claim). This
	// is the exact production scenario: a naive per-mode-independent bound (LEAST(cap, balance)
	// alone) would grant manual the full $30 it asked for, since $30 <= $40 in isolation, letting
	// the two caps sum to $60 against a $40 real account.
	manualAE, err := repo.SetTradingCap(ctx, "manual", decimal.NewFromInt(30))
	if err != nil {
		t.Fatalf("set manual cap: %v", err)
	}
	if !manualAE.EquityUSD.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("manual equity = %s, want 10 (balance 40 - bot's claimed 30)", manualAE.EquityUSD)
	}

	// The stored TradingCapUSD still records the operator's actual REQUEST (30), not the clamped
	// result — SetTradingCap's own doc comment is explicit that trading_cap_usd and equity_usd can
	// differ once a bound applies, the same way a cap exceeding the raw balance already worked
	// before this change. Re-deriving via GetAccountEquity confirms this is what a fresh read sees
	// too, not just the value this one call happened to return.
	manualRead, err := repo.GetAccountEquity(ctx, "manual", decimal.NewFromInt(100))
	if err != nil {
		t.Fatalf("re-read manual account: %v", err)
	}
	if manualRead.TradingCapUSD == nil || !manualRead.TradingCapUSD.Equal(decimal.NewFromInt(30)) {
		t.Fatalf("manual TradingCapUSD = %v, want 30 (the requested cap, even though equity was clamped to 10)", manualRead.TradingCapUSD)
	}
	if !manualRead.EquityUSD.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("manual equity on re-read = %s, want 10", manualRead.EquityUSD)
	}
}

// Releasing the sibling's claim (setting its cap to 0... in practice the smallest a cap can go,
// since SetTradingCap has no notion of "unset") must free that room back up for the other mode on
// its NEXT cap change — the bound reads the sibling's CURRENT equity_usd fresh every call, not a
// value captured once. Directly exercises that the two modes' caps interact both ways: manual
// shrinking its own claim must let bot claim more on its next call, not just the reverse direction
// TestSetTradingCap_BoundsAgainstSiblingModesClaim already covers.
func TestSetTradingCap_SiblingReleasingClaimFreesRoomOnNextCall(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	botBefore, err := repo.GetAccountEquity(ctx, "bot", decimal.NewFromInt(100))
	if err != nil {
		t.Fatalf("read bot account: %v", err)
	}
	manualBefore, err := repo.GetAccountEquity(ctx, "manual", decimal.NewFromInt(100))
	if err != nil {
		t.Fatalf("read manual account: %v", err)
	}
	t.Cleanup(func() {
		if botBefore.TradingCapUSD != nil {
			_, _ = repo.SetTradingCap(ctx, "bot", *botBefore.TradingCapUSD)
		} else {
			_, _ = repo.SetTradingCap(ctx, "bot", botBefore.AccountBalanceUSD)
		}
		if manualBefore.TradingCapUSD != nil {
			_, _ = repo.SetTradingCap(ctx, "manual", *manualBefore.TradingCapUSD)
		} else {
			_, _ = repo.SetTradingCap(ctx, "manual", manualBefore.AccountBalanceUSD)
		}
	})

	balance := decimal.NewFromInt(50)
	if _, err := repo.RecordExchangeBalance(ctx, "bot", balance, decimal.Zero, ""); err != nil {
		t.Fatalf("seed bot balance: %v", err)
	}
	if _, err := repo.RecordExchangeBalance(ctx, "manual", balance, decimal.Zero, ""); err != nil {
		t.Fatalf("seed manual balance: %v", err)
	}

	// Manual claims $40 of the $50 balance, leaving bot only $10 of room.
	if _, err := repo.SetTradingCap(ctx, "manual", decimal.NewFromInt(40)); err != nil {
		t.Fatalf("set manual cap to 40: %v", err)
	}
	botAE, err := repo.SetTradingCap(ctx, "bot", decimal.NewFromInt(40))
	if err != nil {
		t.Fatalf("set bot cap to 40: %v", err)
	}
	if !botAE.EquityUSD.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("bot equity = %s, want 10 (50 balance - manual's 40 claim)", botAE.EquityUSD)
	}

	// Manual releases most of its claim, down to $5 — bot's NEXT cap call must see that fresh
	// number, not the $40 it saw a moment ago.
	if _, err := repo.SetTradingCap(ctx, "manual", decimal.NewFromInt(5)); err != nil {
		t.Fatalf("set manual cap to 5: %v", err)
	}
	botAE, err = repo.SetTradingCap(ctx, "bot", decimal.NewFromInt(40))
	if err != nil {
		t.Fatalf("set bot cap to 40 again: %v", err)
	}
	if !botAE.EquityUSD.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("bot equity after manual released its claim = %s, want 40 (50 balance - manual's now-5 claim, "+
			"clamped to the requested 40)", botAE.EquityUSD)
	}
}
