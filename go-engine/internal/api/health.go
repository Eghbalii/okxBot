package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// Service health for the panel, built after two outages that looked identical from the outside and
// had completely different causes (CLAUDE.md §47, §48).
//
// The panel could not tell them apart, which is the whole problem this answers:
//
//   - §47: the container was CRASH-LOOPING. Every restart hit an unbuildable strategy row and
//     exited, so Docker's DNS could not resolve it and the panel reported
//     "dial tcp: lookup trader ... no such host".
//   - §48: the container was UP and had HALTED ITSELF on a false drift signal, and stayed halted
//     for two hours after the condition had cleared.
//
// From the panel both read as "real trading isn't working". The first needs a fix and a rebuild;
// the second needs a reset. Guessing wrong wastes time in one direction and hides a real problem in
// the other — so this reports the distinction directly rather than leaving it to be inferred.

// serviceHealth is one service's state.
type serviceHealth struct {
	Name      string `json:"name"`
	Container string `json:"container"`
	// State is Docker's own word: running / restarting / exited / created / paused. "restarting" is
	// the crash-loop signal and is deliberately not flattened into "down".
	State  string `json:"state"`
	Status string `json:"status"`
	Health string `json:"health,omitempty"`
	// Critical marks services whose failure stops trading, so the panel can rank them above the
	// monitoring tier rather than showing sixteen equal rows.
	Critical bool `json:"critical"`
}

// haltStatus reports the real-trading circuit breaker (CLAUDE.md §48's "what is a halt").
type haltStatus struct {
	// Halted is whether new positions are currently blocked. Note this never stops monitoring or
	// closing EXISTING positions — a halt bounds new risk, it does not abandon open risk.
	Halted bool   `json:"halted"`
	Reason string `json:"reason,omitempty"`
	// SafeToReset is the whole point of this endpoint, and why the panel's reset button is gated on
	// it rather than on a confirmation dialog. Clearing a halt that is still justified resumes
	// trading against state this system knows to be wrong — which is strictly worse than staying
	// halted. So cmd/api re-derives the condition from the exchange and the database itself, and
	// the button only unlocks when the evidence says it has cleared.
	SafeToReset bool `json:"safeToReset"`
	// Blockers explains why it is not safe yet, in the operator's terms. An empty list alongside
	// SafeToReset=false means the check itself could not run — reported as its own blocker rather
	// than silently reading as "safe".
	Blockers []string `json:"blockers,omitempty"`
	// Evidence is what the check actually compared, so the operator can judge for themselves
	// instead of trusting a boolean.
	ExchangePositions int `json:"exchangePositions"`
	LocalOpenOrders   int `json:"localOpenOrders"`
}

// healthResponse is GET /api/health's payload.
type healthResponse struct {
	Services []serviceHealth `json:"services"`
	Halt     *haltStatus     `json:"halt,omitempty"`
	// DockerError is set when container states could not be read at all, so the panel shows "cannot
	// determine" rather than an empty list that looks like "nothing is running".
	DockerError string `json:"dockerError,omitempty"`
}

// trackedServices is the roster the panel shows, in display order.
//
// Explicit rather than "every container", so a service that is missing entirely is visible as
// missing instead of simply absent from the list — the failure mode where something was never
// started looks exactly like it was never configured.
var trackedServices = []struct {
	Name      string
	Container string
	Critical  bool
}{
	{"Bot Trader", "okxbot-trader-1", true},
	{"Paper trader", "okxbot-paper-trader-1", true},
	{"Ingestor", "okxbot-ingestor-1", true},
	{"OKX gateway", "okxbot-okx-gateway-1", true},
	{"API", "okxbot-api-1", true},
	{"Kafka", "okxbot-kafka-1", true},
	{"TimescaleDB", "okxbot-timescaledb-1", true},
	{"RL service", "okxbot-rl-service-1", true},
	{"Redis", "okxbot-redis-1", false},
	{"Panel", "okxbot-panel-1", false},
	{"Grafana", "okxbot-grafana-1", false},
	{"Prometheus", "okxbot-prometheus-1", false},
	{"Loki", "okxbot-loki-1", false},
}

// handleHealth reports every tracked service's state plus the real-trading halt status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := healthResponse{Services: make([]serviceHealth, 0, len(trackedServices))}

	states, err := listContainers(ctx, newDockerHTTPClient(dockerSocket))
	if err != nil {
		// Reported rather than fatal: the halt half of this response is independently useful, and
		// losing container states must not also cost the operator the reason trading stopped.
		resp.DockerError = err.Error()
	}

	for _, svc := range trackedServices {
		h := serviceHealth{Name: svc.Name, Container: svc.Container, Critical: svc.Critical}
		if st, ok := states[svc.Container]; ok {
			h.State, h.Status, h.Health = st.State, st.Status, st.Health
		} else if resp.DockerError == "" {
			// Docker answered and this container was not in the list — it does not exist, which is
			// different from "could not check" and is worth saying plainly.
			h.State, h.Status = "missing", "not found"
		}
		resp.Services = append(resp.Services, h)
	}

	resp.Halt = s.haltStatus(ctx)
	writeJSON(w, http.StatusOK, resp)
}

// haltStatus derives the real-trading halt state and whether clearing it is safe.
//
// The halt itself lives in cmd/trader's memory (risk.Manager), which cmd/api cannot read. Rather
// than add a cross-process query, this re-derives the CONDITION from the same two sources the
// trader's own reconcile compares: the exchange's open positions and this system's open rows. That
// is deliberately the stronger check — it answers "is the situation actually resolved", which is
// what the operator needs, rather than "does the trader still have a flag set".
func (s *Server) haltStatus(ctx context.Context) *haltStatus {
	out := &haltStatus{}

	if s.Positions == nil {
		out.Blockers = append(out.Blockers,
			"cannot verify: no exchange client configured, so drift cannot be checked")
		return out
	}

	remote, err := s.Positions.GetPositions(s.execInstType())
	if err != nil {
		out.Blockers = append(out.Blockers, fmt.Sprintf("cannot verify: exchange unreachable (%v)", err))
		return out
	}
	open := 0
	for _, p := range remote {
		if !p.Pos.IsZero() {
			open++
		}
	}
	out.ExchangePositions = open

	// ListBotPositions, not ListPositions: real orders live in their own table since §34, and
	// ListPositions reads paper_orders. Using it here reported every real position as untracked —
	// caught against live data on first deploy, where it claimed 2 exchange positions and 0 local
	// rows while the database held both.
	openOnly := true
	local, err := s.Repo.ListBotPositions(ctx, port.PositionFilter{Mode: "bot", Open: &openOnly})
	if err != nil {
		out.Blockers = append(out.Blockers, fmt.Sprintf("cannot verify: database unreadable (%v)", err))
		return out
	}
	out.LocalOpenOrders = len(local)

	// The two halt conditions reconcile can trip on (§27.6): a position on one side that the other
	// does not know about. Both are checked here so the panel names the specific mismatch rather
	// than reporting a generic "not safe".
	if open > len(local) {
		out.Blockers = append(out.Blockers, fmt.Sprintf(
			"exchange reports %d open position(s) but only %d are tracked locally — resolve the "+
				"untracked position before resetting", open, len(local)))
	}
	if len(local) > open {
		out.Blockers = append(out.Blockers, fmt.Sprintf(
			"%d position(s) are open locally but the exchange reports %d — the trader will close "+
				"the stale one on its next reconcile", len(local), open))
	}

	out.SafeToReset = len(out.Blockers) == 0
	return out
}

// execInstType is the instrument type real positions live under, defaulting to FUTURES.
func (s *Server) execInstType() string {
	if strings.TrimSpace(s.ExecInstType) != "" {
		return s.ExecInstType
	}
	return "FUTURES"
}

// positionLister is the narrow exchange capability this file needs: read open positions, nothing
// more. Deliberately not the full port.ExchangeClient — a health endpoint must not be able to place
// or cancel an order, and the type system is a better guarantee of that than care.
type positionLister interface {
	GetPositions(instType string) ([]domain.Position, error)
}

// handleResetHalt clears the real-trading halt, but only when the condition that caused it has
// actually cleared.
//
// The gate is the point (2026-09-13 operator decision): "first we have to know a problem occurred,
// then understand why, and only reset if it is safe." A confirmation dialog does not achieve that —
// it asks the operator to guess. This re-derives the evidence and refuses when the drift is still
// real, so the button cannot resume trading against state the system knows to be wrong.
//
// The reset itself is a restart of cmd/trader. risk.Manager's halted flag lives in that process's
// memory with no cross-process API, and its startup path re-reads positions from the exchange and
// the database anyway — so a restart both clears the flag and re-derives the state it should have,
// which is strictly more correct than poking the flag from outside.
func (s *Server) handleResetHalt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	status := s.haltStatus(ctx)
	if !status.SafeToReset {
		// 409 rather than 400: the request is well formed, the system's state is what refuses it.
		// The blockers are returned so the panel can say WHY instead of just refusing.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    "not safe to reset: the condition that halted trading is still present",
			"blockers": status.Blockers,
			"halt":     status,
		})
		return
	}

	if strings.TrimSpace(s.TraderBaseURL) == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "trader service URL is not configured",
		})
		return
	}

	// Same proxy shape as the existing restart button.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.TraderBaseURL, "/")+"/restart", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A trader that cannot be reached is very often a trader that is crash-looping (§47) rather
		// than halted — a completely different problem, so the message says so instead of leaving
		// the operator to decode a dial error.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("trader unreachable (%v) — if it is restarting rather than halted, "+
				"check its logs: a crash loop needs a fix, not a reset", err),
		})
		return
	}
	defer resp.Body.Close()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "restarting",
		"note":   "the trader is restarting; its halt state is cleared on startup",
	})
}
