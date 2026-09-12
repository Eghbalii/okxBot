package port

// SymbolResolver translates this system's internal short symbol ("BTC") into the identifier one
// exchange's API actually expects.
//
// This is a port rather than a concrete type because the answer is genuinely per-exchange, and the
// difference is not cosmetic:
//
//   - OKX's real-trading-eligible instruments are X-Perp futures whose id embeds a rolling expiry
//     date ("BTC-USD_UM_XPERP-310404", CLAUDE.md §33.2/§33.4). It cannot be derived from "BTC" by
//     any rule, it changes over time as OKX rolls the contract, and it differs per settlement
//     currency — so OKX needs a configured lookup table, and an unmapped symbol must fail loudly.
//   - MEXC's perpetuals are plain "BTC_USDT" (verified live against
//     contract.mexc.com/api/v1/contract/detail, 2026-09-12). A lookup table there would be pure
//     ceremony: a hundred config lines restating a rule, each one a chance to typo an instrument
//     into silence.
//
// Before this port existed, usecase.AffordabilityService held an okx.SymbolMap directly — a
// use-case depending on an adapter, which §10's whole layering exists to prevent, and which would
// have forced MEXC to either fabricate a redundant map or fork the service.
//
// Implementations must FAIL rather than return an empty or guessed identifier. A wrong instrument
// id does not error at the exchange, it succeeds against nothing: the subscription stays silent and
// the pipeline looks healthy, which is the exact failure mode CLAUDE.md §9 records for a mis-cased
// bar name and §33.5 for an unmapped symbol.
type SymbolResolver interface {
	// Resolve returns the exchange-specific instrument id for one internal symbol.
	Resolve(symbol string) (string, error)
	// ResolveAll resolves every symbol in order, failing on the first unresolvable one rather than
	// silently returning a short list — a service must never quietly trade fewer instruments than
	// it was configured for.
	ResolveAll(symbols []string) ([]string, error)
}

// IdentitySymbolResolver is the resolver for exchanges whose instrument ids this system already
// uses verbatim, or whose ids are derived by the adapter itself rather than configured. Resolve is
// the identity function, so no configuration is needed and none can drift out of date.
//
// It still rejects the empty symbol: an empty instrument id is precisely the silent-success failure
// this port's contract exists to prevent, and letting it through here would defeat the check for
// every exchange that uses this implementation.
type IdentitySymbolResolver struct{}

func (IdentitySymbolResolver) Resolve(symbol string) (string, error) {
	if symbol == "" {
		return "", errEmptySymbol
	}
	return symbol, nil
}

func (r IdentitySymbolResolver) ResolveAll(symbols []string) ([]string, error) {
	out := make([]string, len(symbols))
	for i, s := range symbols {
		v, err := r.Resolve(s)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
