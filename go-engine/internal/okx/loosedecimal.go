package okx

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// LooseDecimal is a decimal.Decimal that accepts an EMPTY STRING as zero.
//
// Why this exists (found 2026-09-13 while adding the all-tickers discovery endpoint, and it would
// otherwise have failed silently in production): OKX returns "" — not "0", not a missing field — for
// every price field of an instrument that has never traded. On instType=FUTURES there is exactly one
// such instrument today, TEST002-USD_UM_XPERP-310822, and because the response is decoded as a
// single array, that ONE row failed the decode for all 207 instruments with
// `can't convert "" to decimal`. The whole market scan for the instType real trading executes
// against (CLAUDE.md §33.2) returned nothing at all.
//
// Deliberately its OWN type rather than a change to decimal.Decimal's handling everywhere: on the
// trading path an empty price is a fault and must fail loudly — silently reading a missing fill
// price as zero is how a close gets recorded at the wrong number (CLAUDE.md §37). Tolerance is
// correct only here, where the alternative is discarding a whole exchange's market data because one
// dead instrument has no price.
type LooseDecimal struct {
	decimal.Decimal
}

// UnmarshalJSON accepts OKX's usual quoted number, a bare JSON number, an empty string, or null,
// mapping the latter two to zero.
func (d *LooseDecimal) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == `""` || s == "null" {
		d.Decimal = decimal.Zero
		return nil
	}
	if err := d.Decimal.UnmarshalJSON(b); err != nil {
		return fmt.Errorf("loose decimal %s: %w", s, err)
	}
	return nil
}

// MarshalJSON keeps the embedded decimal's own representation, so a round trip through this type is
// indistinguishable from the plain one.
func (d LooseDecimal) MarshalJSON() ([]byte, error) { return json.Marshal(d.Decimal) }
