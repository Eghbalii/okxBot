package postgres

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// This file covers the exchange-scoping added 2026-09-22 so a second, fully independent
// cmd/paper-trader process (e.g. against MEXC) can run in parallel with the existing OKX
// instance with completely separate stats/PnL/account-balance/config. Run against a REAL
// database, matching this package's own established practice (paper_trading_config_test.go's
// doc comment: a fake repository cannot reproduce Postgres's own SQL/type behavior).

// ListOpenPaperOrders is the CRITICAL isolation point: two engines must never see each other's
// open positions even if instID collides across exchanges. Opens one order under "okx" and one
// under "mexc" for the SAME inst_id, and asserts each exchange's list only sees its own.
func TestListOpenPaperOrders_IsolatesByExchange(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	instID := "TESTEXCHANGEISOLATION"
	okxID, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID: instID, Side: "buy", EntryPx: decimal.RequireFromString("100"),
		Size: decimal.RequireFromString("10"), Leverage: decimal.RequireFromString("1"),
		Mode: "paper", Variant: "baseline", Exchange: "okx",
	})
	if err != nil {
		t.Fatalf("open okx order: %v", err)
	}
	mexcID, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID: instID, Side: "sell", EntryPx: decimal.RequireFromString("200"),
		Size: decimal.RequireFromString("20"), Leverage: decimal.RequireFromString("1"),
		Mode: "paper", Variant: "baseline", Exchange: "mexc",
	})
	if err != nil {
		t.Fatalf("open mexc order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(ctx, `DELETE FROM paper_orders WHERE id = ANY($1)`, []int64{okxID, mexcID})
	})

	okxOpen, err := repo.ListOpenPaperOrders(ctx, instID, "okx")
	if err != nil {
		t.Fatalf("list okx open orders: %v", err)
	}
	if len(okxOpen) != 1 || okxOpen[0].ID != okxID {
		t.Fatalf("okx exchange saw %v, want exactly [%d]", ids(okxOpen), okxID)
	}

	mexcOpen, err := repo.ListOpenPaperOrders(ctx, instID, "mexc")
	if err != nil {
		t.Fatalf("list mexc open orders: %v", err)
	}
	if len(mexcOpen) != 1 || mexcOpen[0].ID != mexcID {
		t.Fatalf("mexc exchange saw %v, want exactly [%d]", ids(mexcOpen), mexcID)
	}

	// exchange="" must default to "okx", matching every pre-existing call site from before this
	// parameter existed — a caller that never heard of a second exchange must keep seeing exactly
	// what it always saw.
	defaultOpen, err := repo.ListOpenPaperOrders(ctx, instID, "")
	if err != nil {
		t.Fatalf("list default-exchange open orders: %v", err)
	}
	if len(defaultOpen) != 1 || defaultOpen[0].ID != okxID {
		t.Fatalf("default (empty) exchange saw %v, want exactly [%d] (okx)", ids(defaultOpen), okxID)
	}
}

func ids(orders []port.PaperOrder) []int64 {
	out := make([]int64, len(orders))
	for i, o := range orders {
		out[i] = o.ID
	}
	return out
}

// GetPaperTradingConfig/SavePaperTradingConfig round-trip independently per (mode, exchange) —
// saving a change under "mexc" must not be visible under "okx", even for the same mode="paper".
func TestPaperTradingConfig_RoundTripsIndependentlyPerExchange(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	okxBefore, err := repo.GetPaperTradingConfig(ctx, "paper", "okx")
	if err != nil {
		t.Fatalf("read okx config: %v", err)
	}
	mexcBefore, err := repo.GetPaperTradingConfig(ctx, "paper", "mexc")
	if err != nil {
		t.Fatalf("read mexc config: %v", err)
	}
	t.Cleanup(func() {
		okxState := okxBefore.TradingState
		_, _ = repo.SavePaperTradingConfig(ctx, "paper", "okx", port.PaperTradingConfigPatch{TradingState: &okxState})
		mexcState := mexcBefore.TradingState
		_, _ = repo.SavePaperTradingConfig(ctx, "paper", "mexc", port.PaperTradingConfigPatch{TradingState: &mexcState})
	})

	paused := "paused"
	if _, err := repo.SavePaperTradingConfig(ctx, "paper", "mexc", port.PaperTradingConfigPatch{TradingState: &paused}); err != nil {
		t.Fatalf("save mexc config: %v", err)
	}

	mexcAfter, err := repo.GetPaperTradingConfig(ctx, "paper", "mexc")
	if err != nil {
		t.Fatalf("re-read mexc config: %v", err)
	}
	if mexcAfter.TradingState != "paused" {
		t.Fatalf("mexc trading state = %q, want paused", mexcAfter.TradingState)
	}

	// The OKX row must be UNTOUCHED by the MEXC save.
	okxAfter, err := repo.GetPaperTradingConfig(ctx, "paper", "okx")
	if err != nil {
		t.Fatalf("re-read okx config: %v", err)
	}
	if okxAfter.TradingState != okxBefore.TradingState {
		t.Fatalf("okx trading state changed from %q to %q after saving MEXC's config — the two exchanges are not isolated",
			okxBefore.TradingState, okxAfter.TradingState)
	}

	// exchange="" must resolve to the same row as "okx" — every pre-existing call site relies on
	// this default.
	defaultAfter, err := repo.GetPaperTradingConfig(ctx, "paper", "")
	if err != nil {
		t.Fatalf("re-read default-exchange config: %v", err)
	}
	if defaultAfter.TradingState != okxAfter.TradingState {
		t.Fatalf("default (empty) exchange read %q, want %q (must default to okx)", defaultAfter.TradingState, okxAfter.TradingState)
	}
}

// TokenStatsAllTime scopes by exchange: a closed trade recorded under "mexc" must not be counted
// in "okx"'s stats for the same inst_id, and vice versa.
func TestTokenStatsAllTime_ScopesByExchange(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	instID := "TESTTOKENSTATSEXCHANGE"
	okxID, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID: instID, Side: "buy", EntryPx: decimal.RequireFromString("100"),
		Size: decimal.RequireFromString("10"), Leverage: decimal.RequireFromString("1"),
		Mode: "paper", Variant: "baseline", Exchange: "okx",
	})
	if err != nil {
		t.Fatalf("open okx order: %v", err)
	}
	mexcID, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID: instID, Side: "buy", EntryPx: decimal.RequireFromString("100"),
		Size: decimal.RequireFromString("10"), Leverage: decimal.RequireFromString("1"),
		Mode: "paper", Variant: "baseline", Exchange: "mexc",
	})
	if err != nil {
		t.Fatalf("open mexc order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(ctx, `DELETE FROM paper_orders WHERE id = ANY($1)`, []int64{okxID, mexcID})
	})

	if err := repo.ClosePaperOrder(ctx, okxID, decimal.RequireFromString("110"), "manual",
		decimal.RequireFromString("1.5"), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("close okx order: %v", err)
	}
	// Close TWO mexc trades so the position counts can't coincidentally match.
	if err := repo.ClosePaperOrder(ctx, mexcID, decimal.RequireFromString("105"), "manual",
		decimal.RequireFromString("0.5"), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("close mexc order: %v", err)
	}

	okxStats, err := repo.TokenStatsAllTime(ctx, "paper", "okx")
	if err != nil {
		t.Fatalf("okx token stats: %v", err)
	}
	mexcStats, err := repo.TokenStatsAllTime(ctx, "paper", "mexc")
	if err != nil {
		t.Fatalf("mexc token stats: %v", err)
	}

	okxCount := countFor(okxStats, instID)
	mexcCount := countFor(mexcStats, instID)
	if okxCount != 1 {
		t.Errorf("okx position count for %s = %d, want 1 (must not include the mexc trade)", instID, okxCount)
	}
	if mexcCount != 1 {
		t.Errorf("mexc position count for %s = %d, want 1 (must not include the okx trade)", instID, mexcCount)
	}
}

func countFor(stats []port.TokenStats, instID string) int64 {
	for _, s := range stats {
		if s.InstID == instID {
			return s.PositionCount
		}
	}
	return 0
}
