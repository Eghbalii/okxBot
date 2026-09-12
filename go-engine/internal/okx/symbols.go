// Package okx's symbols.go is the one translation boundary between this project's internal token
// identity (a short symbol, e.g. "BTC") and the real, full OKX instId a WebSocket subscription or
// REST call actually needs (e.g. "BTC-USD_UM_XPERP-310404") — the operator's explicit design
// (2026-09-04, CLAUDE.md §27): this bot only trades USD-quoted perpetual futures, so there is no
// reason for every service/DB row/config entry to carry OKX's full, expiry-dated instId when a
// short symbol says exactly as much and never needs updating when OKX rolls a contract's expiry.
package okx

import (
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// SymbolMap resolves a short internal symbol ("BTC") to the real OKX instId a WS subscription or
// REST call must use ("BTC-USD_UM_XPERP-310404"). Deliberately just a map, not a computed
// convention: X-Perp instIds carry an expiry date OKX periodically rolls
// (CLAUDE.md §27/§27's exec_inst_id_map precedent), so the real instId for a given symbol changes
// over time in a way no formula can predict — this must be re-verified against a live account and
// updated in config before every real-trading deployment, not derived.
type SymbolMap map[string]string

// Resolve returns symbol's real OKX instId, or an error naming the symbol if it's not in the map
// — a service must never silently subscribe to/call an empty or wrong instId, since that fails
// exactly the way a mis-cased bar name did (CLAUDE.md §9): the channel/endpoint call "succeeds"
// while producing no data, and nothing looks broken until someone notices the gap.
func (m SymbolMap) Resolve(symbol string) (string, error) {
	instID, ok := m[symbol]
	if !ok || instID == "" {
		return "", fmt.Errorf("no OKX instId configured for symbol %q — check trading.symbol_map", symbol)
	}
	return instID, nil
}

// ResolveAll resolves every symbol in symbols, in order, returning an error immediately on the
// first unresolvable one (rather than silently dropping it and continuing with a partial list) —
// a service must never start collecting/trading against fewer instruments than configured without
// a loud failure explaining why.
func (m SymbolMap) ResolveAll(symbols []string) ([]string, error) {
	out := make([]string, len(symbols))
	for i, s := range symbols {
		instID, err := m.Resolve(s)
		if err != nil {
			return nil, err
		}
		out[i] = instID
	}
	return out, nil
}

// Compile-time proof that SymbolMap satisfies the port every use-case depends on. Without this,
// a signature drift in port.SymbolResolver would only surface at the one cmd/ wiring site that
// assigns it, which is exactly the kind of break that hides until deploy.
var _ port.SymbolResolver = SymbolMap(nil)
