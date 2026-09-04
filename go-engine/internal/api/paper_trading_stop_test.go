package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// stopStubRepo is scoped to exactly what handleSavePaperTradingConfig's real-mode Stop path
// exercises (operator decision, 2026-09-04: Stop must close every open real position immediately
// from cmd/api, not wait for cmd/trader to notice trading_state='stopped' at its own restart).
type stopStubRepo struct {
	port.Repository
	savedPatch                     port.PaperTradingConfigPatch
	requestRealManualCloseAllCalls int
	closeAllErr                    error
}

func (s *stopStubRepo) SavePaperTradingConfig(ctx context.Context, mode string, patch port.PaperTradingConfigPatch) (port.PaperTradingConfig, error) {
	s.savedPatch = patch
	return port.PaperTradingConfig{}, nil
}

func (s *stopStubRepo) RequestRealManualCloseAll(ctx context.Context) (int, error) {
	s.requestRealManualCloseAllCalls++
	return 3, s.closeAllErr
}

func doSaveConfig(srv *Server, mode string, tradingState *string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"mode": mode, "tradingState": tradingState})
	req := httptest.NewRequest("PUT", "/api/paper-trading/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleSavePaperTradingConfig(rec, req)
	return rec
}

func strPtr(s string) *string { return &s }

// TestHandleSavePaperTradingConfig_RealStopClosesEveryOpenPositionImmediately confirms the safety-
// critical path: setting mode=real tradingState=stopped calls RequestRealManualCloseAll in the
// SAME request, not merely saving a flag for cmd/trader to notice on its own next restart. This is
// what makes the panel's Stop button an actual emergency stop rather than a delayed one.
func TestHandleSavePaperTradingConfig_RealStopClosesEveryOpenPositionImmediately(t *testing.T) {
	repo := &stopStubRepo{}
	srv := &Server{Repo: repo, Logger: slog.Default()}

	rec := doSaveConfig(srv, "real", strPtr("stopped"))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.requestRealManualCloseAllCalls != 1 {
		t.Errorf("expected RequestRealManualCloseAll called exactly once, got %d", repo.requestRealManualCloseAllCalls)
	}
}

// TestHandleSavePaperTradingConfig_RealPausedDoesNotCloseAnything confirms only "stopped" triggers
// the immediate close sweep — "paused" (existing positions keep running) must not.
func TestHandleSavePaperTradingConfig_RealPausedDoesNotCloseAnything(t *testing.T) {
	repo := &stopStubRepo{}
	srv := &Server{Repo: repo, Logger: slog.Default()}

	rec := doSaveConfig(srv, "real", strPtr("paused"))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.requestRealManualCloseAllCalls != 0 {
		t.Errorf("expected no RequestRealManualCloseAll call for paused, got %d", repo.requestRealManualCloseAllCalls)
	}
}

// TestHandleSavePaperTradingConfig_PaperStopDoesNotCallRealCloseAll confirms mode=paper's own Stop
// keeps using its existing startup-sweep mechanism (cmd/paper-trader's RequestManualCloseAll,
// unrelated to this real-only addition) rather than the real-mode bulk-close path.
func TestHandleSavePaperTradingConfig_PaperStopDoesNotCallRealCloseAll(t *testing.T) {
	repo := &stopStubRepo{}
	srv := &Server{Repo: repo, Logger: slog.Default()}

	rec := doSaveConfig(srv, "paper", strPtr("stopped"))

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.requestRealManualCloseAllCalls != 0 {
		t.Errorf("expected no RequestRealManualCloseAll call for mode=paper, got %d", repo.requestRealManualCloseAllCalls)
	}
}

// TestHandleSavePaperTradingConfig_CloseAllErrorStillSavesConfig confirms a failure to flag
// positions for immediate close does not prevent the config save itself from succeeding (logged,
// not fatal) — the operator still gets the state change recorded even if the close-all sweep
// itself hit an error, and cmd/trader's own next-restart sweep remains a second layer.
func TestHandleSavePaperTradingConfig_CloseAllErrorStillSavesConfig(t *testing.T) {
	repo := &stopStubRepo{closeAllErr: context.DeadlineExceeded}
	srv := &Server{Repo: repo, Logger: slog.Default()}

	rec := doSaveConfig(srv, "real", strPtr("stopped"))

	if rec.Code != 200 {
		t.Fatalf("expected 200 even when the close-all sweep errors, got %d: %s", rec.Code, rec.Body.String())
	}
}
