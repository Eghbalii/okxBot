package rest

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

// TestSideCodeFor covers the translation most likely to cause a real, expensive bug: MEXC encodes
// direction AND intent in one integer (1 open-long, 2 close-short, 3 open-short, 4 close-long)
// where the domain splits them across side + posSide. A wrong mapping here does not error — it
// places a real order in the wrong direction, which is exactly the class of bug CLAUDE.md §14
// records (a sign-unaware comparison silently placed every requested short as a long).
func TestSideCodeFor(t *testing.T) {
	cases := []struct {
		name          string
		side, posSide string
		want          int
	}{
		{"open long (hedge)", "buy", "long", sideOpenLong},
		{"close long (hedge)", "sell", "long", sideCloseLong},
		{"open short (hedge)", "sell", "short", sideOpenShort},
		{"close short (hedge)", "buy", "short", sideCloseShort},
		{"net mode buy opens long", "buy", "", sideOpenLong},
		{"net mode sell opens short", "sell", "", sideOpenShort},
		{"case insensitive", "BUY", "LONG", sideOpenLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sideCodeFor(c.side, c.posSide)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("sideCodeFor(%q,%q) = %d, want %d", c.side, c.posSide, got, c.want)
			}
		})
	}
}

// An unknown side must error rather than default. Defaulting would place a real order in a
// direction nobody asked for.
func TestSideCodeFor_RejectsUnknown(t *testing.T) {
	if _, err := sideCodeFor("sideways", "long"); err == nil {
		t.Error("expected an error for an unknown side")
	}
	if _, err := sideCodeFor("buy", "diagonal"); err == nil {
		t.Error("expected an error for an unknown posSide")
	}
}

// TestOpenTypeFor_DefaultsToIsolated — an unrecognised margin mode must fail SAFE. Isolated bounds
// a liquidation to one position's margin; cross exposes the whole account, which is why §27.2 chose
// isolated. A silent default to cross would be the dangerous direction.
func TestOpenTypeFor_DefaultsToIsolated(t *testing.T) {
	if got := openTypeFor("cross"); got != openTypeCross {
		t.Errorf("openTypeFor(cross) = %d, want %d", got, openTypeCross)
	}
	if got := openTypeFor("isolated"); got != openTypeIsolated {
		t.Errorf("openTypeFor(isolated) = %d, want %d", got, openTypeIsolated)
	}
	for _, unknown := range []string{"", "portfolio", "garbage"} {
		if got := openTypeFor(unknown); got != openTypeIsolated {
			t.Errorf("openTypeFor(%q) = %d, want isolated (%d) — an unknown mode must fail safe",
				unknown, got, openTypeIsolated)
		}
	}
}

// TestDomainState covers the partial-fill distinction MEXC's state code does NOT carry: it reports
// "uncompleted" for both an untouched resting order and a half-filled one, so the difference comes
// from dealVol. §27.5 treats a partial fill as a real, smaller position, and a flatten that only
// partly fills must not be recorded as closed — both depend on this being right.
func TestDomainState(t *testing.T) {
	cases := []struct {
		name    string
		state   int
		dealVol decimal.Decimal
		want    string
	}{
		{"completed is filled", mexcStateCompleted, decimal.NewFromInt(10), "filled"},
		{"cancelled", mexcStateCancelled, decimal.Zero, "canceled"},
		{"invalid is terminal", mexcStateInvalid, decimal.Zero, "canceled"},
		{"uncompleted with no fill is live", mexcStateUncompleted, decimal.Zero, "live"},
		{"uncompleted WITH a fill is partial", mexcStateUncompleted, decimal.NewFromInt(3), "partially_filled"},
		{"uninformed with no fill is live", mexcStateUninformed, decimal.Zero, "live"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := domainState(c.state, c.dealVol)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("domainState(%d, %s) = %q, want %q", c.state, c.dealVol, got, c.want)
			}
		})
	}
}

// An unmapped state must error, never be reported as "live". A dead order reported live makes the
// fill-timeout logic wait out its full timeout on an order that will never fill.
func TestDomainState_RejectsUnknown(t *testing.T) {
	if _, err := domainState(99, decimal.Zero); err == nil {
		t.Error("expected an error for an unmapped state, got a silent mapping")
	}
}

// TestSafeRatio_HandlesZeroDenominator — an unreported initial margin must not panic a position
// poll.
func TestSafeRatio_HandlesZeroDenominator(t *testing.T) {
	if got := safeRatio(decimal.NewFromInt(5), decimal.Zero); !got.IsZero() {
		t.Errorf("safeRatio(5, 0) = %s, want 0", got)
	}
	if got := safeRatio(decimal.NewFromInt(5), decimal.NewFromInt(2)); !got.Equal(decimal.NewFromFloat(2.5)) {
		t.Errorf("safeRatio(5, 2) = %s, want 2.5", got)
	}
}

// TestAlgoState covers how this system learns WHY a position closed. §37 records that without
// ActualSide, an exchange-executed stop-loss was recorded as a manual close priced at entry, which
// understated a real -0.265 loss as -0.018 and fed that wrong number to the model as its reward.
func TestAlgoState(t *testing.T) {
	sl := func(v string) stopOrderResp { return stopOrderResp{State: stopStateExecuted, StopLossPx: mustNum(v)} }
	tp := func(v string) stopOrderResp {
		return stopOrderResp{State: stopStateExecuted, TakeProfitPx: mustNum(v)}
	}

	if state, side := algoState(sl("100")); state != "effective" || side != "sl" {
		t.Errorf("executed stop = (%q,%q), want (effective,sl)", state, side)
	}
	if state, side := algoState(tp("200")); state != "effective" || side != "tp" {
		t.Errorf("executed target = (%q,%q), want (effective,tp)", state, side)
	}
	if state, _ := algoState(stopOrderResp{State: stopStateUntriggered}); state != "live" {
		t.Errorf("untriggered = %q, want live", state)
	}
	if state, _ := algoState(stopOrderResp{State: stopStateCancelled}); state != "canceled" {
		t.Errorf("cancelled = %q, want canceled", state)
	}
	if state, _ := algoState(stopOrderResp{State: stopStateFailed}); state != "order_failed" {
		t.Errorf("failed = %q, want order_failed", state)
	}

	// Both levels set on an executed order is genuinely ambiguous from this endpoint. Empty is
	// returned rather than a guess — §37's own fallback is for the caller to use its own close
	// accounting, which is strictly better than recording a fabricated reason.
	both := stopOrderResp{State: stopStateExecuted, StopLossPx: mustNum("100"), TakeProfitPx: mustNum("200")}
	if state, side := algoState(both); state != "effective" || side != "" {
		t.Errorf("ambiguous executed order = (%q,%q), want (effective,\"\") rather than a guess", state, side)
	}
}

// TestParsePositionID_RejectsGarbage — the protection handle round-trips through the database as a
// string, so a malformed one must error rather than silently becoming position 0, which would
// either fail obscurely or act on an unrelated position.
func TestParsePositionID_RejectsGarbage(t *testing.T) {
	if _, err := parsePositionID(""); err == nil {
		t.Error("expected an error for an empty algo id")
	}
	if _, err := parsePositionID("not-a-number"); err == nil {
		t.Error("expected an error for a malformed algo id")
	}
	got, err := parsePositionID("12345")
	if err != nil || got != 12345 {
		t.Errorf("parsePositionID(12345) = (%d,%v), want (12345,nil)", got, err)
	}
}

func mustNum(s string) json.Number { return json.Number(s) }
