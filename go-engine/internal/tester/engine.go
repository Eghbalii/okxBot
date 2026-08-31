package tester

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// BuildOrder turns a fired signal into an Order ready to insert — fixed notional/leverage (no RL
// sizing here, per the operator's own "size/leverage doesn't matter, start flat" instruction),
// levels resolved from the signal's own percentages/prices via strategy.Signal.ResolveLevels so
// this reuses exactly the same price-resolution logic production strategies already rely on.
//
// maxLossPct caps the REALIZED loss the stop can produce once leverage is applied (2026-08-31
// request: "SL should never allow more than 15% loss, at any leverage, no cap on profit") — a
// strategy's own SL percentage is a raw price distance with no leverage awareness, so at high
// leverage it could otherwise realize far more than the intended loss. Zero disables the cap.
// Deliberately duplicated rather than importing conductor.Clamps (this package's own doc comment:
// it must stay independent of usecase/production code) — the math is small enough that a second
// copy costs less than the coupling would.
func BuildOrder(instID string, versionID int64, bar string, price, notionalUSD, leverage decimal.Decimal, maxLossPct decimal.Decimal, signal strategy.Signal) Order {
	resolved := signal.ResolveLevels(price)
	var slPx, tpPx *decimal.Decimal
	if resolved.SLPx.IsPositive() {
		v := clampSLForLoss(signal.Side == strategy.Buy, price, resolved.SLPx, leverage, maxLossPct)
		slPx = &v
	}
	if resolved.TPPx.IsPositive() {
		v := resolved.TPPx
		tpPx = &v
	}
	return Order{
		InstID:    instID,
		VersionID: versionID,
		Bar:       bar,
		Side:      string(signal.Side),
		EntryPx:   price,
		SLPx:      slPx,
		TPPx:      tpPx,
		Size:      notionalUSD,
		Leverage:  leverage,
		OpenedAt:  time.Now().UTC(),
	}
}

// clampSLForLoss tightens sl toward entry if it would realize more than maxLossPct of margin at
// the given leverage — never widens a tighter stop, only pulls in one that is too permissive. A
// non-positive leverage is treated as 1x and a non-positive maxLossPct disables the clamp
// entirely, matching usecase's equivalent (conductor.Clamps.maxSLDistPctFor).
func clampSLForLoss(long bool, entry, sl, leverage, maxLossPct decimal.Decimal) decimal.Decimal {
	if !maxLossPct.IsPositive() || !entry.IsPositive() {
		return sl
	}
	lev := leverage
	if !lev.IsPositive() {
		lev = decimal.NewFromInt(1)
	}
	maxDist := maxLossPct.Div(lev).Mul(entry)
	if long {
		floor := entry.Sub(maxDist)
		if sl.LessThan(floor) {
			return floor
		}
		return sl
	}
	ceiling := entry.Add(maxDist)
	if sl.GreaterThan(ceiling) {
		return ceiling
	}
	return sl
}

// CloseReasonTimeout marks a tester position force-closed for running past the service's
// max_open_duration (2026-08-30 request) — this service has no in-trade update mechanic at all,
// so unlike production's SL/TP-adjust ratchet, a position here can genuinely sit open forever if
// price never reaches either level. Kept separate from 'sl'/'tp' so a human reading tester_orders
// can tell a real level touch from a housekeeping close.
const CloseReasonTimeout = "timeout"

// IsTimedOut reports whether a position opened at openedAt has run past maxOpenDuration and
// should be force-closed (2026-08-30 request). Pure and stateless, mirroring
// conductor.Conductor.IsTimedOut's shape for production's equivalent check — deliberately not
// shared code, since this service and cmd/paper-trader must stay independent (CLAUDE.md's
// package doc comment).
func IsTimedOut(openedAt, now time.Time, maxOpenDuration time.Duration) bool {
	return now.Sub(openedAt) >= maxOpenDuration
}

// RealizedPnL computes a closed position's PnL in USD from entry/close/size/leverage — identical
// math to usecase.realizedPnL, duplicated here (not imported) because that function is
// package-private to usecase and this service is deliberately independent of it.
func RealizedPnL(side string, entryPx, closePx, size, leverage decimal.Decimal) decimal.Decimal {
	direction := decimal.NewFromInt(1)
	if side == "sell" {
		direction = decimal.NewFromInt(-1)
	}
	return direction.Mul(closePx.Sub(entryPx)).Div(entryPx).Mul(size).Mul(leverage)
}
