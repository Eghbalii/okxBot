package api

import (
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// protectionAmender is the single exchange capability cmd/api needs: moving an already-resting
// SL/TP order's trigger prices. An interface rather than the full port.ExchangeClient so this
// package depends on the one behavior it uses, and so a test can supply a stub without a live
// account — the same shape as affordabilityReporter above it.
type protectionAmender interface {
	AmendAlgoOrder(req domain.AlgoOrderAmend) error
	// GetInstrument supplies the price tick every level must sit on. Without it a percentage-derived
	// price reaches OKX with arbitrary decimals and the amend is rejected as a bare "code=1"
	// (2026-09-10) — which is exactly how a manual SL/TP edit failed from the panel.
	GetInstrument(instType, instID string) (domain.Instrument, error)
}

// execInstID resolves a market-data symbol to the instrument real orders execute against. Real
// orders are placed on a different instId than the symbol they are keyed by (CLAUDE.md §33.4), and
// an amend must name the execution instrument or OKX will not find the order.
func (s *Server) execInstID(symbol string) (string, error) {
	if s.ExecInstIDFor == nil {
		return symbol, nil
	}
	instID, err := s.ExecInstIDFor(symbol)
	if err != nil {
		return "", fmt.Errorf("resolve execution instrument for %s: %w", symbol, err)
	}
	return instID, nil
}

// tickRounder returns the instrument's price tick for rounding, or a zero Instrument when it
// cannot be read. A missing tick leaves prices untouched rather than guessing at one: an unrounded
// price is rejected by the exchange, which is visible, while a wrongly-rounded one would be
// accepted at a level nobody chose.
func (s *Server) tickRounder(instType, symbol string) domain.Instrument {
	if s.Protection == nil {
		return domain.Instrument{}
	}
	instID, err := s.execInstID(symbol)
	if err != nil {
		return domain.Instrument{}
	}
	inst, err := s.Protection.GetInstrument(instType, instID)
	if err != nil {
		s.Logger.Warn("could not read instrument metadata for price rounding",
			"instId", instID, "error", err)
		return domain.Instrument{}
	}
	return inst
}
