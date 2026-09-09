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
