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
// Each leg is independent: an order still open has no close leg, and either lookup can fail on its
// own (a rate limit, an id OKX no longer serves) without invalidating the other. A failed leg
// reports its error in place rather than failing the whole request, so one missing half never
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

// handleOrderExchangeRaw fetches an order's full exchange-side record live rather than serving
// something stored: the stored columns are a deliberate few fields chosen at close time, and the
// point of this endpoint is everything else OKX knows — fees, category, fill timestamps, margin
// details, whatever it adds next.
//
// Real orders only. Paper trading has no exchange leg, so there is nothing to fetch and a request
// for one is a caller mistake worth reporting rather than an empty success.
func (s *Server) handleOrderExchangeRaw(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid order id")
		return
	}
	if s.Exchange == nil {
		writeError(w, http.StatusServiceUnavailable, "no exchange client configured")
		return
	}

	order, err := s.Repo.GetRealOrder(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// The instId OKX knows this order by is the EXECUTION instrument, not the short internal
	// symbol the order row carries (CLAUDE.md §33.4) — asking with "SOL" would return nothing.
	execInstID, err := s.SymbolMap.Resolve(order.InstID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot resolve "+order.InstID+" to an exchange instrument: "+err.Error())
		return
	}

	var out orderRawView
	if order.ExchangeOrderID != nil && *order.ExchangeOrderID != "" {
		out.OpenOrderID = *order.ExchangeOrderID
		raw, err := s.Exchange.GetOrderRaw(execInstID, out.OpenOrderID)
		if err != nil {
			out.OpenError = err.Error()
		} else {
			out.Open = raw
		}
	}
	if order.ExchangeCloseOrderID != nil && *order.ExchangeCloseOrderID != "" {
		out.CloseOrderID = *order.ExchangeCloseOrderID
		raw, err := s.Exchange.GetOrderRaw(execInstID, out.CloseOrderID)
		if err != nil {
			out.CloseError = err.Error()
		} else {
			out.Close = raw
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// exchangeReader is the read-only slice of the exchange client this package needs — declared here
// rather than depending on the full port.ExchangeClient so cmd/api structurally cannot place or
// cancel an order, which is cmd/trader's job alone.
type exchangeReader interface {
	GetOrderRaw(instID, ordID string) (json.RawMessage, error)
}
