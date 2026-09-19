package api

import (
	"net/http"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// manualTradeClient is the exchange capability the manual-trade HTTP surface needs
// (docs/MANUAL_TRADE_PLAN.md §3/§6): reading instrument metadata for the token picker/order-form
// validation, and — once the order-placing endpoints land — setting leverage and placing/amending/
// canceling orders and their protective algo orders. Deliberately a subset of port.ExchangeClient,
// the same narrow-interface convention as protectionAmender/positionLister above: cmd/api's
// gatewayclient instance already implements all of this, this interface just states which part of
// it a manual-trade handler is allowed to touch.
type manualTradeClient interface {
	GetInstrument(instType, instID string) (domain.Instrument, error)
	SetLeverage(req domain.LeverageChange) error
	PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error)
	CancelOrder(instID, ordID string) error
	GetOrder(instID, ordID string) (domain.OrderStatus, error)
	PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error)
	CancelAlgoOrder(instID, algoID string) error
	// GetTicker supplies a reference price for converting an order-ticket SL/TP PERCENT into a
	// price before the open-order intent is written (handleCreateManualOrder) — a market order has
	// no price of its own yet, so the live last-traded price stands in for it.
	GetTicker(instID string) (domain.Ticker, error)
}

type manualInstrumentView struct {
	Symbol     string `json:"symbol"`
	ExecInstID string `json:"execInstId"`
	TickSz     string `json:"tickSz"`
	LotSz      string `json:"lotSz"`
	MinSz      string `json:"minSz"`
	CtVal      string `json:"ctVal"`
}

// handleManualInstrumentLookup answers GET /api/manual/instruments?symbol=<SYMBOL> — the token
// picker's per-token metadata read (docs/MANUAL_TRADE_PLAN.md §3/§6, closing the "no tick size/lot
// size reaches the panel" gap the pre-implementation audit found). Deliberately does NOT filter by
// enabled_real (§8.3: manual trading bypasses that flag — a human choosing a specific token is
// trusted, the flag exists to gate automated assignment, not human intent) and does NOT require the
// symbol to already be in the instruments roster: it resolves execInstId and reads live instrument
// metadata for ANY symbol the operator's ExecInstIDFor mapping can resolve, so the token search is
// not limited to the pre-scanned roster.
func (s *Server) handleManualInstrumentLookup(w http.ResponseWriter, r *http.Request) {
	if s.ManualTrade == nil {
		writeError(w, http.StatusServiceUnavailable, "manual trading is not configured on this deployment")
		return
	}
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		writeError(w, http.StatusBadRequest, "symbol is required")
		return
	}
	instID, err := s.execInstID(symbol)
	if err != nil {
		writeError(w, http.StatusNotFound, "no execution instrument for "+symbol)
		return
	}
	inst, err := s.ManualTrade.GetInstrument(s.ExecInstType, instID)
	if err != nil {
		s.Logger.Warn("manual instrument lookup failed", "symbol", symbol, "instId", instID, "error", err)
		writeError(w, http.StatusBadGateway, "could not read instrument from the exchange")
		return
	}
	writeJSON(w, http.StatusOK, manualInstrumentView{
		Symbol:     symbol,
		ExecInstID: instID,
		TickSz:     inst.TickSz.String(),
		LotSz:      inst.LotSz.String(),
		MinSz:      inst.MinSz.String(),
		CtVal:      inst.CtVal.String(),
	})
}
