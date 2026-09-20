package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// ManualTradingConfig configures the one order-adjacent exchange call this package makes directly
// (SetLeverage) — mirrors cfg.Trading.TdMode/PosMode, set once at cmd/api startup. Every actual
// order-opening/closing/adjusting decision happens in cmd/trader's ManualTrader
// (docs/MANUAL_TRADE_PLAN.md §1/§4), which reads the same config values independently.
type ManualTradingConfig struct {
	TdMode  string
	PosMode string
}

// manualLeverageRequest is POST /api/manual/leverage's body (docs/MANUAL_TRADE_PLAN.md §3).
type manualLeverageRequest struct {
	Symbol   string          `json:"symbol"`
	Leverage decimal.Decimal `json:"leverage"`
	// Side is required only in hedge mode (PosMode="long_short"), to pick which side's leverage is
	// being set — OKX's set-leverage call needs PosSide in that mode, same as an order itself does.
	Side string `json:"side,omitempty"`
}

// handleManualSetLeverage answers POST /api/manual/leverage: {symbol, leverage, side?} -> sets the
// exchange's per-instrument leverage before the operator opens a position (docs/MANUAL_TRADE_PLAN.md
// §3). Real-money leverage changes go through the same manualTradeClient (-> gatewayclient ->
// cmd/okx-gateway) every other real-mode mutation uses, so they're visible/logged/rate-limited
// identically — nothing about this endpoint bypasses the gateway's "one process holds credentials"
// property (CLAUDE.md §27.1).
func (s *Server) handleManualSetLeverage(w http.ResponseWriter, r *http.Request) {
	if s.ManualTrade == nil {
		writeError(w, http.StatusServiceUnavailable, "manual trading is not configured on this deployment")
		return
	}
	var req manualLeverageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Symbol == "" {
		writeError(w, http.StatusBadRequest, "symbol is required")
		return
	}
	if !req.Leverage.IsPositive() {
		writeError(w, http.StatusBadRequest, "leverage must be a positive number")
		return
	}
	instID, err := s.execInstID(req.Symbol)
	if err != nil {
		writeError(w, http.StatusNotFound, "no execution instrument for "+req.Symbol)
		return
	}
	change := domain.LeverageChange{InstID: instID, Lever: req.Leverage, MgnMode: s.ManualTrading.TdMode}
	if s.ManualTrading.PosMode == "long_short" {
		if req.Side != "buy" && req.Side != "sell" {
			writeError(w, http.StatusBadRequest, "side (buy or sell) is required in hedge mode")
			return
		}
		change.PosSide = manualPosSideFor(req.Side)
	}
	if err := s.ManualTrade.SetLeverage(change); err != nil {
		writeError(w, http.StatusBadGateway, "the exchange rejected the leverage change: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// manualAccountModeResponse is GET /api/manual/account-mode's shape — reports the account's
// CURRENT position mode plus whether the precondition for switching TO hedge mode holds right now
// (zero open positions), so the panel can preemptively disable that option with an accurate reason
// rather than only discovering it's blocked after a rejected exchange call.
type manualAccountModeResponse struct {
	PosMode           string `json:"posMode"` // "net" or "hedge" — this codebase's own short form
	OpenPositionCount int    `json:"openPositionCount"`
	CanSwitchToHedge  bool   `json:"canSwitchToHedge"`
}

// posModeFromOKX/posModeToOKX translate between OKX's own wire values ("net_mode"/
// "long_short_mode") and this codebase's shorter "net"/"hedge" convention used everywhere else
// (config.Trading.PosMode uses "net"/"long_short" — yet another spelling; the panel gets the
// shortest, clearest one rather than propagating either exchange-specific string to the UI).
func posModeFromOKX(wire string) string {
	if wire == "long_short_mode" {
		return "hedge"
	}
	return "net"
}

func posModeToOKX(short string) (string, error) {
	switch short {
	case "hedge":
		return "long_short_mode", nil
	case "net":
		return "net_mode", nil
	default:
		return "", fmt.Errorf("posMode must be net or hedge, got %q", short)
	}
}

// handleGetManualAccountMode answers GET /api/manual/account-mode.
func (s *Server) handleGetManualAccountMode(w http.ResponseWriter, r *http.Request) {
	if s.ManualTrade == nil {
		writeError(w, http.StatusServiceUnavailable, "manual trading is not configured on this deployment")
		return
	}
	cfg, err := s.ManualTrade.GetAccountConfig()
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read account config: "+err.Error())
		return
	}
	positions, err := s.ManualTrade.GetPositions(s.ExecInstType)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read positions: "+err.Error())
		return
	}
	open := 0
	for _, p := range positions {
		if !p.Pos.IsZero() {
			open++
		}
	}
	writeJSON(w, http.StatusOK, manualAccountModeResponse{
		PosMode:           posModeFromOKX(cfg.PosMode),
		OpenPositionCount: open,
		CanSwitchToHedge:  open == 0,
	})
}

// manualSetAccountModeRequest is POST /api/manual/account-mode's body.
type manualSetAccountModeRequest struct {
	PosMode string `json:"posMode"` // "net" or "hedge"
}

// handleSetManualAccountMode answers POST /api/manual/account-mode — switches the account's
// position mode. Real-money account-wide exchange call, so this pre-checks the same zero-open-
// positions precondition OKX itself enforces (CLAUDE.md §27.6/§49.2's "ask the exchange, don't
// just trust local state" — the pre-check reads live positions rather than any local cache) before
// ever sending the request, so a blocked switch fails with a clear reason instead of a bare
// exchange rejection. The exchange's own answer stays authoritative either way: a race between this
// check and a position opening a moment later still surfaces as the exchange's own error.
func (s *Server) handleSetManualAccountMode(w http.ResponseWriter, r *http.Request) {
	if s.ManualTrade == nil {
		writeError(w, http.StatusServiceUnavailable, "manual trading is not configured on this deployment")
		return
	}
	var req manualSetAccountModeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	wireMode, err := posModeToOKX(req.PosMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.PosMode == "hedge" {
		positions, err := s.ManualTrade.GetPositions(s.ExecInstType)
		if err != nil {
			writeError(w, http.StatusBadGateway, "could not read positions: "+err.Error())
			return
		}
		for _, p := range positions {
			if !p.Pos.IsZero() {
				writeError(w, http.StatusConflict, "cannot switch to hedge mode while any position is open — close every position first")
				return
			}
		}
	}
	if err := s.ManualTrade.SetPositionMode(wireMode); err != nil {
		writeError(w, http.StatusBadGateway, "the exchange rejected the position-mode change: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// manualPosSideFor mirrors usecase's own posSideFor/signedNotionalForSide (unexported there, not
// reachable from this package) for the one hedge-mode decision this file needs to make on its own:
// a buy opens/adds to "long", a sell opens/adds to "short". Every actual order placement still
// happens in cmd/trader's ManualTrader, which resolves this the same way independently — this
// helper exists only for the leverage pre-set call, which needs to name a side before any order
// exists to derive one from.
func manualPosSideFor(side string) string {
	if side == "sell" {
		return "short"
	}
	return "long"
}

// manualOrderRequest is POST /api/manual/orders' body (docs/MANUAL_TRADE_PLAN.md §3). SL/TP accept
// either an absolute price or a signed margin percentage (matching handleAdjustPosition's own
// priceFromMarginPct convention) — never both for the same level, and percent is converted to a
// price here so ManualTrader's intent row always carries a plain price, keeping the price the one
// thing every downstream reader (ManualTrader, the panel) has to understand.
type manualOrderRequest struct {
	Symbol    string           `json:"instId"`
	Side      string           `json:"side"`
	OrderType string           `json:"orderType"` // "market" or "limit"; empty defaults to market
	LimitPx   *decimal.Decimal `json:"limitPx"`
	SizeUSD   decimal.Decimal  `json:"sizeUsd"`
	Leverage  decimal.Decimal  `json:"leverage"`
	SLPx      *decimal.Decimal `json:"slPx"`
	SLPct     *float64         `json:"slPct"`
	TPPx      *decimal.Decimal `json:"tpPx"`
	TPPct     *float64         `json:"tpPct"`
	// TdMode is a per-order margin-mode choice ("cross" or "isolated") — OKX already accepts this
	// per-request on every order/leverage call (domain.OrderRequest.TdMode), so this needs no
	// account-wide exchange call the way position mode does. Empty defaults to "cross".
	TdMode string `json:"tdMode"`
}

// resolveLevel converts a manual order request's price-or-percent SL/TP field into a single price,
// reusing priceFromMarginPct so the conversion can never disagree with handleAdjustPosition's own
// (CLAUDE.md real-trading readiness plan §3b) — refPx is the entry for a market order or the given
// limit price for a limit order, since that IS the price the level should be measured from either
// way. Both px and pct set is a caller error (ambiguous which one wins); neither set is fine (no
// level requested).
func resolveLevel(px *decimal.Decimal, pct *float64, refPx, leverage decimal.Decimal, side string) (*decimal.Decimal, error) {
	switch {
	case px != nil && pct != nil:
		return nil, errBothPriceAndPercent
	case px != nil:
		return px, nil
	case pct != nil:
		p := priceFromMarginPct(refPx, leverage, side, decimal.NewFromFloat(*pct))
		return &p, nil
	default:
		return nil, nil
	}
}

var errBothPriceAndPercent = &manualRequestError{"specify a level as a price or a percent, not both"}

type manualRequestError struct{ msg string }

func (e *manualRequestError) Error() string { return e.msg }

// handleCreateManualOrder answers POST /api/manual/orders — writes an open-order INTENT row
// (docs/MANUAL_TRADE_PLAN.md §2.3/§4) and returns immediately; cmd/trader's ManualTrader is the
// only thing that ever calls PlaceOrder for it, on its own next poll (~1-2s). This keeps "only one
// process holds credentials and talks to the exchange" intact (CLAUDE.md §27.1) — cmd/api never
// calls PlaceOrder itself, and never sees the account-wide reconcile-halt failure mode a direct
// call would risk (CLAUDE.md §48, and the plan's own §1 "rejected alternative").
//
// A reference price is needed here ONLY to convert an SLPct/TPPct into a price before the intent is
// written (so ManualTrader's own open sequence always sees a plain price, never a percent it would
// have to re-derive with its own, possibly-different, reference price) — the live ticker for a
// market order, or the operator's own limit price for a limit order, matching resolveLevel's own
// reasoning.
func (s *Server) handleCreateManualOrder(w http.ResponseWriter, r *http.Request) {
	if s.ManualTrade == nil {
		writeError(w, http.StatusServiceUnavailable, "manual trading is not configured on this deployment")
		return
	}
	var req manualOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Symbol == "" {
		writeError(w, http.StatusBadRequest, "instId is required")
		return
	}
	if req.Side != "buy" && req.Side != "sell" {
		writeError(w, http.StatusBadRequest, "side must be buy or sell")
		return
	}
	orderType := req.OrderType
	if orderType == "" {
		orderType = "market"
	}
	if orderType != "market" && orderType != "limit" {
		writeError(w, http.StatusBadRequest, "orderType must be market or limit")
		return
	}
	if orderType == "limit" && (req.LimitPx == nil || !req.LimitPx.IsPositive()) {
		writeError(w, http.StatusBadRequest, "limitPx is required and must be positive for a limit order")
		return
	}
	if !req.SizeUSD.IsPositive() {
		writeError(w, http.StatusBadRequest, "sizeUsd must be a positive number")
		return
	}
	if !req.Leverage.IsPositive() {
		writeError(w, http.StatusBadRequest, "leverage must be a positive number")
		return
	}
	tdMode := req.TdMode
	if tdMode == "" {
		tdMode = "cross"
	}
	if tdMode != "cross" && tdMode != "isolated" {
		writeError(w, http.StatusBadRequest, "tdMode must be cross or isolated")
		return
	}

	instID, err := s.execInstID(req.Symbol)
	if err != nil {
		writeError(w, http.StatusNotFound, "no execution instrument for "+req.Symbol)
		return
	}

	refPx := decimal.Zero
	if orderType == "limit" {
		refPx = *req.LimitPx
	} else {
		ticker, err := s.ManualTrade.GetTicker(instID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "could not read the current price: "+err.Error())
			return
		}
		if !ticker.Last.IsPositive() {
			writeError(w, http.StatusBadGateway, "exchange reported no usable price for "+req.Symbol)
			return
		}
		refPx = ticker.Last
	}

	slPx, err := resolveLevel(req.SLPx, req.SLPct, refPx, req.Leverage, req.Side)
	if err != nil {
		writeError(w, http.StatusBadRequest, "sl: "+err.Error())
		return
	}
	tpPx, err := resolveLevel(req.TPPx, req.TPPct, refPx, req.Leverage, req.Side)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tp: "+err.Error())
		return
	}

	intent := port.ManualOrderIntent{
		InstID:    req.Symbol,
		Side:      req.Side,
		OrderType: orderType,
		LimitPx:   req.LimitPx,
		SizeUSD:   req.SizeUSD,
		Leverage:  req.Leverage,
		SLPx:      slPx,
		TPPx:      tpPx,
		TdMode:    tdMode,
	}
	id, err := s.Repo.CreateManualOrderIntent(r.Context(), intent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int64{"intentId": id})
}

// handleGetManualOrderIntent answers GET /api/manual/order-intents/{id} — the panel polls this
// while an order is still being placed (status pending/claimed), before a manual_orders row exists
// (docs/MANUAL_TRADE_PLAN.md §2.3). Named order-intents rather than orders/intent/{id} to avoid an
// ambiguous route registration against GET /api/manual/orders/{id}/adjustments — net/http's
// ServeMux rejects two patterns of the same segment-depth where one has a literal and the other a
// wildcard at the same position with more segments following.
func (s *Server) handleGetManualOrderIntent(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in, err := s.Repo.GetManualOrderIntent(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// manualOrdersListResponse mirrors positionsListResponse's shape for the manual-orders table.
type manualOrdersListResponse struct {
	Items []port.ManualOrder `json:"items"`
	Total int                `json:"total"`
}

// handleListManualOrders answers GET /api/manual/orders?open=true|false — lists manual orders for
// the panel's Trade page (docs/MANUAL_TRADE_PLAN.md §3). No mode filter (every row here is real by
// construction, matching ListManualOrders' own PositionFilter.Mode-is-ignored contract).
func (s *Server) handleListManualOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := port.PositionFilter{InstID: q.Get("instId"), SortBy: q.Get("sortBy"), SortDesc: q.Get("sortDesc") == "true"}
	if v := q.Get("open"); v != "" {
		open := v == "true"
		filter.Open = &open
	}
	orders, err := s.Repo.ListManualOrders(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, manualOrdersListResponse{Items: orders, Total: len(orders)})
}

// handleGetManualOrder answers GET /api/manual/orders/{id} — a single order, for the panel's order
// ticket to poll once it knows the id (from the intent's ManualOrderID once claimed).
func (s *Server) handleGetManualOrder(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	o, err := s.Repo.GetManualOrder(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// handleCloseManualOrder answers POST /api/manual/orders/{id}/close — a FILLED/PARTIAL order only
// (docs/MANUAL_TRADE_PLAN.md §8.1: a still-resting limit order is canceled instead, see
// handleCancelManualOrder). Async intent, same pattern as RequestBotManualClose: ManualTrader
// flattens it on its own next sweep.
func (s *Server) handleCloseManualOrder(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Repo.RequestManualOrderClose(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleCancelManualOrder answers POST /api/manual/orders/{id}/cancel — a still-RESTING (unfilled
// limit) order only (docs/MANUAL_TRADE_PLAN.md §8.1). Canceling a resting order is a different
// exchange call and a different terminal state (close_reason='canceled', no position ever existed)
// from closing a filled one, so this is a separate endpoint rather than overloading close.
func (s *Server) handleCancelManualOrder(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Repo.CancelManualOrder(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleAdjustManualOrder answers POST /api/manual/orders/{id}/adjust — same
// {slPct?, tpPct?} shape as handleAdjustPosition, and the SAME "deliberately unclamped" reasoning
// (CLAUDE.md real-trading readiness plan §3b): a human operator acting directly on their own manual
// position is trusted the way nothing else automated in this codebase is. Amends the exchange
// FIRST, exactly like handleAdjustPosition, unless the order is protected-by-strategy (§8.4) — in
// that case there is no algo order this order itself owns to amend, and the panel must not let the
// operator believe editing "their slice" changes a level that is actually the strategy's.
func (s *Server) handleAdjustManualOrder(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req adjustPositionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.SLPct == nil && req.TPPct == nil {
		writeError(w, http.StatusBadRequest, "at least one of slPct/tpPct is required")
		return
	}

	o, err := s.Repo.GetManualOrder(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if o.ClosedAt != nil {
		writeError(w, http.StatusConflict, "order is already closed")
		return
	}
	if o.EntryPx == nil || !o.EntryPx.IsPositive() {
		writeError(w, http.StatusConflict, "order has no entry price to adjust from (still resting, unfilled)")
		return
	}
	if o.ProtectedByStrategy {
		writeError(w, http.StatusConflict,
			"this position shares its exchange protection with an open strategy position; "+
				"close or take over the strategy's position first to set an independent SL/TP")
		return
	}
	if o.ExchangeAlgoOrderID == nil || *o.ExchangeAlgoOrderID == "" {
		writeError(w, http.StatusConflict,
			"this position has no resting stop/target on the exchange to move; it may still be opening")
		return
	}
	if s.Protection == nil {
		writeError(w, http.StatusServiceUnavailable,
			"no exchange connection configured; refusing to change levels the exchange would not receive")
		return
	}

	newSL, newTP := o.SLPx, o.TPPx
	if req.SLPct != nil {
		px := priceFromMarginPct(*o.EntryPx, o.Leverage, o.Side, decimal.NewFromFloat(*req.SLPct))
		newSL = &px
	}
	if req.TPPct != nil {
		px := priceFromMarginPct(*o.EntryPx, o.Leverage, o.Side, decimal.NewFromFloat(*req.TPPct))
		newTP = &px
	}

	amend := domain.AlgoOrderAmend{AlgoID: *o.ExchangeAlgoOrderID, InstID: o.ExecInstID}
	inst := s.tickRounder(s.ExecInstType, o.InstID)
	if newSL != nil && newSL.IsPositive() {
		rounded := inst.RoundPriceToTick(*newSL)
		newSL = &rounded
		amend.SLTriggerPx = rounded
	}
	if newTP != nil && newTP.IsPositive() {
		rounded := inst.RoundPriceToTick(*newTP)
		newTP = &rounded
		amend.TPTriggerPx = rounded
	}
	if err := s.Protection.AmendAlgoOrder(amend); err != nil {
		writeError(w, http.StatusBadGateway, "the exchange rejected the new levels, nothing was changed: "+err.Error())
		return
	}

	if err := s.Repo.SetManualOrderProtection(r.Context(), id, o.ExchangeAlgoOrderID, false); err != nil {
		s.Logger.Warn("adjust manual order: could not refresh protection record", "id", id, "error", err)
	}
	if req.SLPct != nil && !samePriceOrNil(newSL, o.SLPx) {
		if err := s.Repo.RecordManualOrderAdjustment(r.Context(), id, "sl", o.SLPx, newSL); err != nil {
			s.Logger.Warn("adjust manual order: record sl adjustment failed", "id", id, "error", err)
		}
	}
	if req.TPPct != nil && !samePriceOrNil(newTP, o.TPPx) {
		if err := s.Repo.RecordManualOrderAdjustment(r.Context(), id, "tp", o.TPPx, newTP); err != nil {
			s.Logger.Warn("adjust manual order: record tp adjustment failed", "id", id, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"slPx": newSL, "tpPx": newTP})
}

// handleListManualOrderAdjustments answers GET /api/manual/orders/{id}/adjustments — the order
// ticket's SL/TP edit history, mirroring handleListPaperOrderAdjustments' shape exactly.
func (s *Server) handleListManualOrderAdjustments(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	adjustments, err := s.Repo.ListManualOrderAdjustments(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, adjustments)
}
