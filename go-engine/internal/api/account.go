package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// defaultEquityHistoryLimit bounds an unqualified history read so a long-running paper account
// can't return an unbounded row count to the panel's chart. The panel can ask for more explicitly.
const defaultEquityHistoryLimit = 1000

// validModes are the trading modes an account balance is tracked for (CLAUDE.md §15.6). "manual"
// added 2026-09-20 (Account page): manual/discretionary trading gets its own trading-cap
// bookkeeping alongside bot trading, both slices of the same real exchange balance. Validated here
// rather than passed through, so a typo'd mode is a 400 instead of silently seeding a new account
// row for a mode nothing ever trades against.
var validModes = map[string]bool{"paper": true, "bot": true, "manual": true}

// realMoneyModes mirrors internal/postgres's own list — every mode with a live exchange balance
// behind it, as opposed to "paper"'s fictional one. Used to route requests to the operation each
// mode actually needs (SetTradingCap vs. SetAccountCap, see handleSetAccountCap's own doc comment).
var realMoneyModes = map[string]bool{"bot": true, "manual": true}

// queryMode resolves the ?mode= parameter, defaulting to "paper" (the only mode with a live writer
// today, CLAUDE.md §11.4). Reports false after writing a 400 if the mode isn't recognized.
func queryMode(w http.ResponseWriter, r *http.Request) (string, bool) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		return "paper", true
	}
	if !validModes[mode] {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper, bot, or manual): "+mode)
		return "", false
	}
	return mode, true
}

// handleGetAccount returns one mode's current shared-account balance — equity, its configured
// starting point, and how many times a drain has reset it (CLAUDE.md §15.6/§15.7). The reset count
// is the signal worth watching: an account resetting daily is a failing configuration, not free
// money, and surfacing it is the whole reason it's tracked.
func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	mode, ok := queryMode(w, r)
	if !ok {
		return
	}

	// exchange ("okx", "mexc", ...) only means anything for mode="paper" today — bot/manual
	// trading has no second-exchange instance (2026-09-22). Empty defaults to "okx" at the
	// repository layer, unchanged for every existing panel request that never sends it.
	account, err := s.Repo.GetAccountEquityEx(r.Context(), mode, r.URL.Query().Get("exchange"), s.AccountInitialUSD)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// setAccountCapRequest is POST /api/account/cap's body.
type setAccountCapRequest struct {
	// Mode defaults to "paper" when omitted, same as the ?mode= query params elsewhere in this
	// file — a body field rather than a query param since this is a mutating POST, not a GET.
	Mode      string          `json:"mode"`
	NewCapUSD decimal.Decimal `json:"newCapUsd"`
	// Exchange ("okx", "mexc", ...) only means anything for mode="paper" (2026-09-22) —
	// empty defaults to "okx" at the repository layer, unchanged for every existing panel request.
	Exchange string `json:"exchange"`
}

// handleSetAccountCap lets the operator explicitly DEPOSIT/WITHDRAW to bring a mode's real balance
// to a chosen value — e.g. "I've decided to trade with $40 from this point on" (CLAUDE.md §31.2/
// §31.3). This moves BOTH InitialUSD/EquityUSD ("Total Equity" — resets to the chosen baseline)
// AND AccountBalanceUSD ("Account Balance" — the real continuous total) to the same new value,
// bumps ResetCount, and writes a reason="reset" history row. Moving only EquityUSD (the first,
// buggy version of this path) left Balance sitting below Equity by a gap no real trade produced —
// mathematically incoherent, since this schema never carries unrealized PnL separately, so outside
// of realized-PnL deltas the two fields must stay equal.
//
// Real money is NOT excluded here the way it's excluded from ApplyRealizedPnL's automatic
// drain-to-zero reset (CLAUDE.md §15.7's real-mode carve-out) — that carve-out exists to stop an
// AUTOMATIC top-up from ever happening to real capital after a loss. This endpoint is the opposite
// case: an operator explicitly depositing/withdrawing on their own real account is a decision only
// a human makes, exactly the "a human decision, not a bookkeeping event" framing §15.7 itself
// describes for real mode — it is not a value this handler should second-guess by refusing the mode.
func (s *Server) handleSetAccountCap(w http.ResponseWriter, r *http.Request) {
	var req setAccountCapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "paper"
	}
	if !validModes[mode] {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper, bot, or manual): "+mode)
		return
	}
	// 0 is a legitimate, explicit cap ("give this mode nothing of the shared balance") — only a
	// negative request is rejected. Was IsPositive() (rejecting 0 too) until 2026-09-20, when an
	// operator correctly pointed out that reducing a mode's cap back to 0 is a real, valid action
	// (e.g. "manual gets nothing for now"), not an error.
	if req.NewCapUSD.IsNegative() {
		writeError(w, http.StatusBadRequest, "newCapUsd must not be negative")
		return
	}

	// Real and paper mean genuinely different things by "set a cap", so this routes to two
	// different repository operations rather than one with a mode branch inside it (2026-09-08):
	//
	//   paper        — there is no exchange, so AccountBalanceUSD is bookkeeping this system owns
	//                  and a cap is a re-baselining of the whole account. Unchanged (CLAUDE.md
	//                  §32.3).
	//   bot / manual — AccountBalanceUSD mirrors the exchange's own reported balance and is
	//                  RecordExchangeBalance's reconciliation anchor, so it must NEVER be
	//                  overwritten with a chosen number: the next poll would report the
	//                  difference as realized PnL that never happened. The cap instead names the
	//                  tradable slice of that balance, and the untraded remainder is a derived
	//                  reserve. "manual" added 2026-09-20 — it shares the SAME real balance as
	//                  "bot" (Repository.SetTradingCap's own doc comment covers how the two caps
	//                  are bounded against each other so they can never jointly exceed it).
	var account port.AccountEquity
	var err error
	if realMoneyModes[mode] {
		account, err = s.Repo.SetTradingCap(r.Context(), mode, req.NewCapUSD)
	} else {
		account, err = s.Repo.SetAccountCapEx(r.Context(), mode, req.Exchange, req.NewCapUSD)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// handleAccountHistory returns one mode's balance timeline, oldest-first, for the panel's chart
// (CLAUDE.md §15.7). Each point carries the reason it was written ("trade", "reset", "seed"), which
// is what lets the chart mark a drain-and-reset distinctly from ordinary trade PnL.
func (s *Server) handleAccountHistory(w http.ResponseWriter, r *http.Request) {
	mode, ok := queryMode(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	// Only means anything for mode="paper" (2026-09-22) — empty defaults to "okx" at the
	// repository layer, unchanged for every existing panel request that never sends it.
	exchange := q.Get("exchange")

	var since time.Time
	if v := q.Get("since"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since (want RFC3339): "+err.Error())
			return
		}
		since = parsed
	} else if account, err := s.Repo.GetAccountEquityEx(r.Context(), mode, exchange, s.AccountInitialUSD); err == nil && account.LastResetAt != nil {
		// CLAUDE.md §31.2: with no explicit since, the chart shows the balance's story since the
		// operator last chose a baseline (SetAccountCap) or a drain auto-reset happened — not the
		// account's entire lifetime, which may span sizing regimes with nothing to do with the
		// currently-chosen cap. A caller that genuinely wants the full history can still pass an
		// explicit ?since= far enough back.
		since = *account.LastResetAt
	}

	limit := defaultEquityHistoryLimit
	if v := q.Get("limit"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit (want a non-negative integer)")
			return
		}
		limit = parsed
	}

	points, err := s.Repo.ListEquityHistoryEx(r.Context(), mode, exchange, since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, points)
}
