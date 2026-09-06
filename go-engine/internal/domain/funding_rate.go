package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// FundingRate is one settled 8-hour funding period, as OKX reports it (2026-09-06). Live-checked
// against BTC-USD_UM_XPERP: real rates run ~-0.02% to -0.03% per 8h on this project's actual
// X-Perp instruments — negative, meaning shorts pay longs on this product, the OPPOSITE sign
// measured the same day on the standard BTC-USD-SWAP market — and a normal-to-volatile market can
// swing the magnitude roughly 60x (0.005% to 0.3% per 8h). A single config constant cannot track
// either the sign or the magnitude correctly, which is why this is polled and stored rather than
// assumed.
type FundingRate struct {
	InstID       string
	FundingTime  time.Time
	FundingRate  decimal.Decimal // signed fraction, e.g. -0.0002 = -0.02%
}
