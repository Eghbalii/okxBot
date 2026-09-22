package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// statsStubRepo backs handlePaperTradingStats' tests specifically — accountStubRepo
// (account_test.go) already covers GetAccountEquity/ListEquityHistory/SetTradingCap/SetAccountCap;
// this embeds a similar shape plus the position-count methods the stats handler also calls.
type statsStubRepo struct {
	port.Repository
	accountResult port.AccountEquity
	manualOrders  []port.ManualOrder
	botPositions  []port.BotOrder
}

func (r *statsStubRepo) GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	return r.accountResult, nil
}

func (r *statsStubRepo) ListEquityHistory(ctx context.Context, mode string, since time.Time, limit int) ([]port.EquityPoint, error) {
	return nil, nil
}

// GetAccountEquityEx/ListEquityHistoryEx mirror the un-scoped stubs above — handlePaperTradingStats
// switched to these exchange-scoped methods (2026-09-22, multi-exchange paper trading); the test's
// callers never pass a real exchange, so these behave identically to their un-scoped siblings.
func (r *statsStubRepo) GetAccountEquityEx(ctx context.Context, mode, exchange string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	return r.accountResult, nil
}

func (r *statsStubRepo) ListEquityHistoryEx(ctx context.Context, mode, exchange string, since time.Time, limit int) ([]port.EquityPoint, error) {
	return nil, nil
}

func (r *statsStubRepo) ListManualOrders(ctx context.Context, f port.PositionFilter) ([]port.ManualOrder, error) {
	return r.manualOrders, nil
}

func (r *statsStubRepo) ListBotPositions(ctx context.Context, f port.PositionFilter) ([]port.BotOrder, error) {
	return r.botPositions, nil
}

// statsMode must recognize "manual" alongside the existing "paper"/"bot" — the regression this
// guards is exactly the one found while building the Account page: PositionsTable already served
// mode=manual (a route added earlier), but this handler's own mode validation had never been
// updated to match, so a manual stats request would have 400'd the moment anything called it.
func TestStatsMode_AcceptsManual(t *testing.T) {
	mode, ok := statsMode("manual")
	if !ok || mode != "manual" {
		t.Fatalf("statsMode(\"manual\") = (%q, %v), want (\"manual\", true)", mode, ok)
	}
}

func TestStatsMode_RejectsUnknown(t *testing.T) {
	if _, ok := statsMode("bogus"); ok {
		t.Fatal("statsMode(\"bogus\") should be rejected")
	}
}

// handlePaperTradingStats with mode=manual must count from manual_orders (via CountManualOrders),
// never from paper_orders or bot_orders — manual trading is its own table (CLAUDE.md real-trading
// readiness plan), and a request for its stats reading the wrong table would silently report
// paper's or bot's open-position count instead of manual's own.
func TestHandlePaperTradingStats_ManualModeCountsFromManualOrders(t *testing.T) {
	repo := &statsStubRepo{
		manualOrders: []port.ManualOrder{{ID: 1, Size: dec("5")}, {ID: 2, Size: dec("3")}},
		botPositions: []port.BotOrder{{ID: 99}, {ID: 100}, {ID: 101}}, // deliberately a different count
		accountResult: port.AccountEquity{
			Mode:              "manual",
			EquityUSD:         dec("20"),
			AccountBalanceUSD: dec("40"),
		},
	}
	srv := &Server{Repo: repo}

	req := httptest.NewRequest("GET", "/api/paper-trading/stats?mode=manual", nil)
	rec := httptest.NewRecorder()
	srv.handlePaperTradingStats(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body paperTradingStatsView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.OpenCount != 2 {
		t.Fatalf("OpenCount = %d, want 2 (from manual_orders, not bot_orders' 3)", body.OpenCount)
	}
	if body.TotalEquityUSD != "20" {
		t.Fatalf("TotalEquityUSD = %s, want 20", body.TotalEquityUSD)
	}
	if body.AccountBalanceUSD != "40" {
		t.Fatalf("AccountBalanceUSD = %s, want 40", body.AccountBalanceUSD)
	}
	if body.UsedMarginUSD != "8" {
		t.Fatalf("UsedMarginUSD = %s, want 8 (5 + 3, the two open manual orders' margin)", body.UsedMarginUSD)
	}
	if body.AvailableMarginUSD != "12" {
		t.Fatalf("AvailableMarginUSD = %s, want 12 (20 cap - 8 used)", body.AvailableMarginUSD)
	}
}

// AvailableMarginUSD must floor at zero rather than go negative when open margin exceeds the
// current cap (e.g. the cap was lowered after positions were already opened against a larger one)
// — a sizing UI reading a negative "available" figure would be a confusing, actionable-looking
// number for a state that just means "nothing left."
func TestHandlePaperTradingStats_AvailableMarginFlooredAtZero(t *testing.T) {
	repo := &statsStubRepo{
		manualOrders: []port.ManualOrder{{ID: 1, Size: dec("30")}},
		accountResult: port.AccountEquity{
			Mode:              "manual",
			EquityUSD:         dec("20"),
			AccountBalanceUSD: dec("40"),
		},
	}
	srv := &Server{Repo: repo}

	req := httptest.NewRequest("GET", "/api/paper-trading/stats?mode=manual", nil)
	rec := httptest.NewRecorder()
	srv.handlePaperTradingStats(rec, req)

	var body paperTradingStatsView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.AvailableMarginUSD != "0" {
		t.Fatalf("AvailableMarginUSD = %s, want 0 (floored, not negative)", body.AvailableMarginUSD)
	}
}
