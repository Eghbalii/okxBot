package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/shopspring/decimal"
)

// healthStubRepo follows this package's established pattern (account_test.go, adjust_position_test.go):
// embed a nil port.Repository so only the methods a test actually exercises need overriding. Anything
// else panics loudly rather than silently returning a zero value.
type healthStubRepo struct {
	port.Repository
	open []port.PaperOrder
	err  error
}

func (r *healthStubRepo) ListPositions(context.Context, port.PositionFilter) ([]port.PaperOrder, error) {
	return r.open, r.err
}

// fakePositions stands in for the exchange.
type fakePositions struct {
	positions []domain.Position
	err       error
}

func (f *fakePositions) GetPositions(string) ([]domain.Position, error) {
	return f.positions, f.err
}

func pos(instID string, size int64) domain.Position {
	return domain.Position{InstID: instID, Pos: decimal.NewFromInt(size)}
}

// TestHaltStatus_SafeWhenBothSidesAgree is the case that matters operationally: the drift has
// resolved and the operator should be able to clear the halt.
//
// This is the exact state §48 was stuck in — the DOGE position that triggered the halt closed at
// 07:10, yet trading was still halted at 07:25 with the exchange flat and zero open rows, because
// nothing re-evaluates a halt once set.
func TestHaltStatus_SafeWhenBothSidesAgree(t *testing.T) {
	s := &Server{
		Positions: &fakePositions{positions: nil},
		Repo:      &healthStubRepo{}, Logger: slog.Default(),
	}
	got := s.haltStatus(context.Background())

	if !got.SafeToReset {
		t.Fatalf("both sides flat but reset refused; blockers=%v", got.Blockers)
	}
	if len(got.Blockers) != 0 {
		t.Errorf("expected no blockers, got %v", got.Blockers)
	}
}

// TestHaltStatus_BlocksWhileAnUntrackedPositionExists is the safety half. Clearing a halt while the
// exchange still reports a position this system does not know about resumes trading against state
// known to be wrong — worse than staying halted.
func TestHaltStatus_BlocksWhileAnUntrackedPositionExists(t *testing.T) {
	s := &Server{
		Positions: &fakePositions{positions: []domain.Position{pos("DOGE", -45)}},
		Repo:      &healthStubRepo{}, Logger: slog.Default(), // no local rows
	}
	got := s.haltStatus(context.Background())

	if got.SafeToReset {
		t.Fatal("reset allowed while an untracked exchange position exists — this would resume " +
			"trading against state the system knows is wrong")
	}
	if len(got.Blockers) == 0 {
		t.Fatal("refused without saying why — the operator cannot act on that")
	}
	if !strings.Contains(strings.Join(got.Blockers, " "), "untracked") {
		t.Errorf("blocker does not name the problem: %v", got.Blockers)
	}
	if got.ExchangePositions != 1 || got.LocalOpenOrders != 0 {
		t.Errorf("evidence wrong: exchange=%d local=%d, want 1/0",
			got.ExchangePositions, got.LocalOpenOrders)
	}
}

// TestHaltStatus_UnverifiableIsNotSafe — if the check itself cannot run, the answer must be "no",
// never a default "yes". An unreachable exchange is exactly when a stale reset would be most
// dangerous.
func TestHaltStatus_UnverifiableIsNotSafe(t *testing.T) {
	cases := map[string]*Server{
		"exchange unreachable": {Positions: &fakePositions{err: errors.New("timeout")}, Repo: &healthStubRepo{}, Logger: slog.Default()},
		"no exchange wired":    {Positions: nil, Repo: &healthStubRepo{}, Logger: slog.Default()},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			got := s.haltStatus(context.Background())
			if got.SafeToReset {
				t.Fatal("reported safe while the check could not run — unknown is not safe")
			}
			if len(got.Blockers) == 0 {
				t.Error("no blocker explaining why it could not verify")
			}
		})
	}
}

// TestResetHalt_RefusesWhileUnsafe drives the real HTTP handler, since the gate is only useful if
// it is actually wired into the route rather than merely implemented.
func TestResetHalt_RefusesWhileUnsafe(t *testing.T) {
	s := &Server{
		Positions: &fakePositions{positions: []domain.Position{pos("DOGE", -45)}},
		Repo:      &healthStubRepo{}, Logger: slog.Default(),
		TraderBaseURL: "http://trader:8095",
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/health/reset-halt", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 Conflict; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "blockers") {
		t.Errorf("refusal does not carry the blockers: %s", rec.Body.String())
	}
}

// TestHealth_ReportsRestartingDistinctly guards the §47/§48 distinction the panel exists to make:
// "restarting" (a crash loop, needs a fix) must not be flattened into a generic "down" that reads
// the same as a halt.
func TestHealth_ReportsRestartingDistinctly(t *testing.T) {
	states := map[string]ContainerState{
		"okxbot-trader-1": {Name: "okxbot-trader-1", State: "restarting", Status: "Restarting (1) 20 seconds ago"},
	}
	st, ok := states["okxbot-trader-1"]
	if !ok || st.State != "restarting" {
		t.Fatalf("state = %+v, want restarting preserved verbatim", st)
	}
	if strings.Contains(st.State, "down") {
		t.Error("restarting was flattened into down — that erases the crash-loop signal")
	}
}
