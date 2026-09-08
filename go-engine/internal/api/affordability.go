package api

import (
	"context"
	"net/http"

	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// tokenAffordabilityView is one row of GET /api/paper-trading/affordability — what the panel's
// Manage Tokens modal shows so an operator can see WHY a token is disabled (2026-09-08 request).
type tokenAffordabilityView struct {
	InstID string `json:"instId"`
	// MinNotionalUSD is the smallest position the exchange will accept for this instrument:
	// CtVal * price * MinSz. This is the number that makes a token untradeable on a small account,
	// and the reason it needs to be visible rather than inferred from a declined order.
	MinNotionalUSD string `json:"minNotionalUsd"`
	// BudgetUSD is the per-token budget (equity/activeTokenCount, bounded by max_position_pct) —
	// the same figure sizing uses, so the comparison the panel renders is the real one.
	BudgetUSD string `json:"budgetUsd"`
	// Affordable is BudgetUSD >= MinNotionalUSD at the CURRENT budget. Note a disabled token can
	// read affordable and still be correctly disabled: the budget it is compared against is the
	// one produced by excluding it, and re-admitting it would dilute that budget below its own
	// minimum. PEPE does exactly this on a $20 account — $3.65 minimum against a $4.00 budget at
	// 5 active tokens, but only $3.33 once it becomes the 6th. AutoDisabled below is the field to
	// read for "why is this off", not this one.
	Affordable bool `json:"affordable"`
	// AutoDisabled is true when the affordability service is what turned this token off, as
	// opposed to an operator. That is the distinction the panel's tag draws, and it cannot be
	// derived from Affordable alone for the reason above.
	AutoDisabled bool `json:"autoDisabled"`
	Disabled   bool `json:"disabled"`
	// Unknown marks a token whose instrument or price could not be read. Such a token is never
	// auto-disabled (a transient API failure must not take a tradeable token offline), and the
	// panel shows it as unknown rather than implying it was judged.
	Unknown bool `json:"unknown"`
}

// handleTokenAffordability reports, per token, the exchange's minimum tradeable notional against
// the current per-token budget — the data behind the Manage Tokens modal's min-size column and its
// "auto-disabled" tag.
//
// Real mode only: paper trading has no exchange minimums to respect (it simulates fills at any
// size), so the question is meaningless there and a paper request returns an empty list rather
// than inventing numbers.
func (s *Server) handleTokenAffordability(w http.ResponseWriter, r *http.Request) {
	mode, ok := statsMode(r.URL.Query().Get("mode"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or real)")
		return
	}
	if mode != "real" || s.Affordability == nil {
		writeJSON(w, http.StatusOK, []tokenAffordabilityView{})
		return
	}

	report, err := s.Affordability.Report(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	views := make([]tokenAffordabilityView, 0, len(report.Tokens))
	for _, t := range report.Tokens {
		v := tokenAffordabilityView{
			InstID:    t.InstID,
			BudgetUSD: report.BudgetUSD.StringFixed(4),
			Disabled:  t.Disabled,
			Unknown:   t.Unknown,
		}
		if t.Unknown {
			v.MinNotionalUSD = ""
		} else {
			v.MinNotionalUSD = t.MinNotionalUSD.StringFixed(4)
			v.Affordable = t.Affordable
			v.AutoDisabled = t.AutoDisabled
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, views)
}

// affordabilityReporter is the subset of usecase.AffordabilityService cmd/api needs — an
// interface rather than the concrete type so this package depends on the behavior, and so a test
// can supply a stub without an exchange client or a live account.
type affordabilityReporter interface {
	Report(ctx context.Context) (usecase.AffordabilityReport, error)
}
