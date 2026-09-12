// Package mexc adapts MEXC's futures API to this project's exchange-agnostic ports.
//
// Structure mirrors internal/okx deliberately: rest/ for the signed REST client, ws/ for the
// reconnecting WebSocket clients, and this file for the port assertions that tie them to
// internal/port. A contributor who has read one adapter can navigate the other.
package mexc

import (
	"github.com/eghbalii/okxBot/go-engine/internal/mexc/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// Compile-time proof that the MEXC REST client satisfies the exchange port.
//
// This assertion is the entire point of the exercise, and it is what a second exchange was meant to
// prove: if port.ExchangeClient had been OKX-shaped rather than a real abstraction, this line would
// not compile. It also means a future change to the port that only OKX can satisfy breaks the build
// here rather than at some distant call site — or worse, at runtime on a half-migrated deploy.
var _ port.ExchangeClient = (*rest.Client)(nil)

// SymbolResolver for MEXC is the identity function.
//
// MEXC's perpetual ids are plain "BTC_USDT" — no expiry date, no settlement-currency variant, no
// rolling contract (verified live, 2026-09-13). So unlike OKX, which needs a configured table
// because "BTC-USD_UM_XPERP-310404" is not derivable from "BTC" and changes when the contract rolls
// (§33.4), MEXC needs no mapping at all.
//
// Exposed as a named function rather than leaving callers to reach for port.IdentitySymbolResolver
// themselves, so the REASON is documented at the point a reader looks for MEXC's symbol handling
// and finds, correctly, that there is none.
func NewSymbolResolver() port.SymbolResolver { return port.IdentitySymbolResolver{} }
