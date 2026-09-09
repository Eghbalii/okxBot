package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// orderRawView is GET /api/positions/{id}/exchange-order's response: OKX's own payload for both
// legs of a real position, untouched (2026-09-09 request — "show me the whole JSON, not a few
// parameters you picked").
//
// Each leg is independent: an order still open has no close leg, and a leg whose record was never
// captured reports that in place rather than failing the whole request, so one missing half never
// hides the half that IS available.
type orderRawView struct {
	// Open/Close are OKX's raw order objects, nil when that leg has no recorded order id or its
	// lookup failed (see the matching Error field).
	Open  json.RawMessage `json:"open"`
	Close json.RawMessage `json:"close"`

	OpenError  string `json:"openError,omitempty"`
	CloseError string `json:"closeError,omitempty"`

	// The ids these were fetched with, echoed so the view can show what was asked for even when
	// the lookup itself failed.
	OpenOrderID  string `json:"openOrderId,omitempty"`
	CloseOrderID string `json:"closeOrderId,omitempty"`
}

// handleOrderExchangeRaw serves the exchange records already captured on the order (2026-09-09
// request: "don't call it each time — when the system reads it, save the JSON there").
//
// The engine captures each leg once, at the moment that leg reached a terminal state and its
// record was final — which it was already fetching anyway as part of fill confirmation. So this is
// a plain row read: no exchange call, no rate-limit budget spent, and the same bytes every time
// rather than a record that could differ between views.
//
// Real orders only. Paper trading has no exchange leg, so a request for one is a caller mistake
// worth reporting rather than an empty success.
func (s *Server) handleOrderExchangeRaw(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order id")
		return
	}

	order, err := s.Repo.GetRealOrder(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	out := orderRawView{
		Open:  order.ExchangeOpenRaw,
		Close: order.ExchangeCloseRaw,
	}
	if order.ExchangeOrderID != nil {
		out.OpenOrderID = *order.ExchangeOrderID
	}
	if order.ExchangeCloseOrderID != nil {
		out.CloseOrderID = *order.ExchangeCloseOrderID
	}
	// An id with no stored record is worth distinguishing from no id at all: the first means the
	// capture failed or predates it being captured, the second is simply a leg that does not exist
	// yet (a still-open position has no close order).
	if out.OpenOrderID != "" && len(out.Open) == 0 {
		out.OpenError = "no exchange record was captured for this order"
	}
	if out.CloseOrderID != "" && len(out.Close) == 0 {
		out.CloseError = "no exchange record was captured for this order"
	}

	writeJSON(w, http.StatusOK, out)
}
