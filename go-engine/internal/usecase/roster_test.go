package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

func configFallback() (symbols []string, execIDs map[string]string) {
	return []string{"BTC", "ETH"}, map[string]string{
		"BTC": "BTC-USD_UM_XPERP-310404",
		"ETH": "ETH-USD_UM_XPERP-310404",
	}
}

// TestRosterFor_SeedsFromConfigWhenTheTableIsEmpty covers the deploy that introduces this feature:
// every existing installation has an empty instruments table, and the services must come up trading
// exactly what they traded before rather than nothing.
func TestRosterFor_SeedsFromConfigWhenTheTableIsEmpty(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	syms, ids := configFallback()

	got, err := RosterFor(ctx, repo, "okx", "ingest", syms, ids, "FUTURES", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 2 || got.Symbols[0] != "BTC" {
		t.Fatalf("symbols = %v, want the configured pair", got.Symbols)
	}
	if got.ExecInstID["BTC"] != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("exec id = %q — the roster must carry what replaces symbol_map", got.ExecInstID["BTC"])
	}

	// The seed must be durable, not just returned: the whole point is that a later scan and the
	// panel both read the same table.
	rows, _ := repo.ListInstruments(ctx, port.InstrumentFilter{})
	if len(rows) != 2 {
		t.Fatalf("seeded %d rows, want 2", len(rows))
	}
	for _, in := range rows {
		if in.Source != "seed" {
			t.Errorf("%s: source = %q, want seed so a reader can tell it from a person's choice", in.Symbol, in.Source)
		}
		// Unlike a SCANNED token, a seeded one keeps real enabled: these are the tokens real trading
		// was already configured for, and silently disabling them during a migration would be a
		// behavior change dressed up as a data move.
		if !in.EnabledReal {
			t.Errorf("%s: enabled_real false — seeding must not silently disable real trading", in.Symbol)
		}
	}
}

// The critical boundary: a roster that EXISTS but is fully disabled must be honored, not treated as
// empty. Falling back to config there would resurrect tokens an operator deliberately turned off —
// migration 000030's exact bug, in a new place.
//
// The assertion is that SeedRoster is not CALLED, not merely that the flags survive. Those are
// different claims, and asserting the weaker one proves nothing about this function: UpsertInstrument
// deliberately never overwrites an existing row's flags, so a roster re-seeded on every read would
// still come back disabled and the test would pass against the bug. (It did — found by mutating
// `len(all) == 0` to `len(all) >= 0` and watching the first version of this test stay green.) The
// upsert counter distinguishes them.
func TestRosterFor_DoesNotFallBackWhenEveryRowIsDisabled(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	syms, ids := configFallback()
	if err := SeedRoster(ctx, repo, "okx", syms, ids, "FUTURES"); err != nil {
		t.Fatal(err)
	}

	rows, _ := repo.ListInstruments(ctx, port.InstrumentFilter{})
	off := false
	for _, in := range rows {
		if err := repo.SetInstrumentFlags(ctx, in.ID, port.InstrumentPatch{EnabledIngest: &off}); err != nil {
			t.Fatal(err)
		}
	}

	before := repo.upsertInstrumentCalls
	got, err := RosterFor(ctx, repo, "okx", "ingest", syms, ids, "FUTURES", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 0 {
		t.Errorf("symbols = %v — a fully disabled roster was overridden by the config fallback", got.Symbols)
	}
	if repo.upsertInstrumentCalls != before {
		t.Errorf("RosterFor wrote %d instrument rows — an existing roster must never be re-seeded, whatever its flags say",
			repo.upsertInstrumentCalls-before)
	}
}

// The scenario that broke on the real deploy, minutes after it shipped (2026-09-13).
//
// The discovery scan runs before the ingestor and paper-trader restart, so it populates the roster
// first. The original rule ("seed only when the table is empty") then became unreachable, and two
// CONFIGURED tokens that happened to be below the scan's volume floor that day — TRUMP and PEPE —
// would have silently stopped being collected on the next ingestor restart. A silent data gap on a
// pipeline that looks healthy, which is the exact failure mode §9 exists to prevent.
func TestRosterFor_SeedsConfiguredSymbolsMissedByAScan(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()

	// The scan got there first, and found tokens the operator never configured while missing two the
	// operator did.
	for _, sym := range []string{"BTC", "FIL", "TAO"} {
		if _, _, err := repo.UpsertInstrument(ctx, port.Instrument{
			Symbol: sym, Exchange: "okx", ExecInstID: sym + "-X", InstType: "FUTURES",
			EnabledIngest: true, EnabledPaper: true, Source: "scan",
		}); err != nil {
			t.Fatal(err)
		}
	}

	configured := []string{"BTC", "TRUMP", "PEPE"}
	execIDs := map[string]string{"BTC": "BTC-X", "TRUMP": "TRUMP-X", "PEPE": "PEPE-X"}

	got, err := RosterFor(ctx, repo, "okx", "ingest", configured, execIDs, "FUTURES", nil)
	if err != nil {
		t.Fatal(err)
	}

	have := map[string]bool{}
	for _, s := range got.Symbols {
		have[s] = true
	}
	for _, sym := range configured {
		if !have[sym] {
			t.Errorf("configured symbol %q is missing from the roster — it would silently stop being collected", sym)
		}
	}
	// The scan's own finds must survive too: seeding is additive, not a reset to the config list.
	for _, sym := range []string{"FIL", "TAO"} {
		if !have[sym] {
			t.Errorf("scan-discovered symbol %q was dropped by the seed", sym)
		}
	}
}

// A configured symbol whose row EXISTS but is disabled must stay disabled — seeding fills gaps, it
// does not overrule a person. Without this, the order-independence fix above would have reintroduced
// migration 000030's bug in the process of fixing a different one.
func TestRosterFor_DoesNotReseedADisabledConfiguredSymbol(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, _, err := repo.UpsertInstrument(ctx, port.Instrument{
		Symbol: "BTC", Exchange: "okx", ExecInstID: "BTC-X",
		EnabledIngest: false, EnabledPaper: false, Source: "seed",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := RosterFor(ctx, repo, "okx", "ingest",
		[]string{"BTC"}, map[string]string{"BTC": "BTC-X"}, "FUTURES", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 0 {
		t.Errorf("symbols = %v — a deliberately disabled token was re-enabled by the config seed", got.Symbols)
	}
}

// The three flags are independent (migration 000031's whole design), so a token can be collecting
// data and paper-trading while real trading does not see it at all.
func TestRosterFor_RespectsEachConsumerIndependently(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	// A scanned token: ingest + paper on, real off — exactly what MarketScanner.admit writes.
	if _, _, err := repo.UpsertInstrument(ctx, port.Instrument{
		Symbol: "NEW", Exchange: "okx", ExecInstID: "NEW-X", InstType: "FUTURES",
		EnabledIngest: true, EnabledPaper: true, EnabledReal: false, Source: "scan",
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		consumer string
		want     int
	}{{"ingest", 1}, {"paper", 1}, {"real", 0}} {
		got, err := RosterFor(ctx, repo, "okx", tc.consumer, nil, nil, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.consumer, err)
		}
		if len(got.Symbols) != tc.want {
			t.Errorf("%s: %d symbols, want %d — a scanned token must reach data collection and paper trading but not real money",
				tc.consumer, len(got.Symbols), tc.want)
		}
	}
}

// An unknown consumer name must error rather than quietly return nothing, which would look like a
// quiet market rather than a wiring mistake.
func TestRosterFor_RejectsAnUnknownConsumer(t *testing.T) {
	repo := newFakeRepository()
	repo.UpsertInstrument(context.Background(), port.Instrument{
		Symbol: "BTC", Exchange: "okx", ExecInstID: "BTC-X", EnabledIngest: true,
	})
	if _, err := RosterFor(context.Background(), repo, "okx", "ingset", nil, nil, "", nil); err == nil {
		t.Error("a typo'd consumer name must fail loudly, not return an empty roster")
	}
}

// A row with no exec id is skipped, never subscribed to as an empty string — OKX accepts that and
// then pushes nothing, which is a silent data gap on a pipeline that looks healthy (§9).
func TestRosterFor_SkipsARowWithNoExecID(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	repo.UpsertInstrument(ctx, port.Instrument{Symbol: "GOOD", Exchange: "okx", ExecInstID: "GOOD-X", EnabledIngest: true})
	repo.UpsertInstrument(ctx, port.Instrument{Symbol: "BAD", Exchange: "okx", ExecInstID: "", EnabledIngest: true})

	got, err := RosterFor(ctx, repo, "okx", "ingest", nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 1 || got.Symbols[0] != "GOOD" {
		t.Errorf("symbols = %v, want only GOOD", got.Symbols)
	}
}

// SeedRoster refuses a symbol with no exec id rather than writing an unusable row: config validation
// already requires a symbol_map entry for each, so reaching that state means the two disagree.
func TestSeedRoster_RefusesASymbolWithNoExecID(t *testing.T) {
	err := SeedRoster(context.Background(), newFakeRepository(), "okx",
		[]string{"BTC", "MYSTERY"}, map[string]string{"BTC": "BTC-X"}, "FUTURES")
	if err == nil {
		t.Error("seeding a symbol with no exec instId must fail rather than write an unusable row")
	}
}

func TestRosterWatcher_FiresOnAnAddedToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	repo := newFakeRepository()
	repo.UpsertInstrument(ctx, port.Instrument{Symbol: "BTC", Exchange: "okx", ExecInstID: "BTC-X", EnabledIngest: true})

	fired := make(chan string, 1)
	w := &RosterWatcher{
		Repo: repo, Exchange: "okx", Consumer: "ingest",
		Interval: 10 * time.Millisecond, Baseline: []string{"BTC"},
		OnChange: func(reason string) { fired <- reason },
	}
	// The scan admitting a new token, while the service is already running.
	repo.UpsertInstrument(ctx, port.Instrument{Symbol: "NEW", Exchange: "okx", ExecInstID: "NEW-X", EnabledIngest: true})

	go w.Run(ctx)
	select {
	case reason := <-fired:
		if reason == "" {
			t.Error("OnChange must say what changed")
		}
	case <-ctx.Done():
		t.Fatal("watcher never noticed the added token")
	}
}

// An unchanged roster must NOT restart the service. This is the mutation that matters most: a
// watcher that fires spuriously would put a trading service into a restart loop.
func TestRosterWatcher_DoesNotFireOnAnUnchangedRoster(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	repo := newFakeRepository()
	repo.UpsertInstrument(ctx, port.Instrument{Symbol: "BTC", Exchange: "okx", ExecInstID: "BTC-X", EnabledIngest: true})

	fired := make(chan string, 1)
	w := &RosterWatcher{
		Repo: repo, Exchange: "okx", Consumer: "ingest",
		Interval: 10 * time.Millisecond, Baseline: []string{"BTC"},
		OnChange: func(reason string) { fired <- reason },
	}
	go w.Run(ctx)

	select {
	case reason := <-fired:
		t.Fatalf("watcher restarted a healthy service: %s", reason)
	case <-ctx.Done():
	}
}

// An empty read is treated as unreadable rather than as "every token was removed" — restarting into
// an empty roster would crash-loop.
func TestRosterWatcher_DoesNotFireOnAnEmptyRoster(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	fired := make(chan string, 1)
	w := &RosterWatcher{
		Repo: newFakeRepository(), Exchange: "okx", Consumer: "ingest",
		Interval: 10 * time.Millisecond, Baseline: []string{"BTC"},
		OnChange: func(reason string) { fired <- reason },
	}
	go w.Run(ctx)

	select {
	case reason := <-fired:
		t.Fatalf("an empty roster must not trigger a restart: %s", reason)
	case <-ctx.Done():
	}
}

func TestDiffSymbols(t *testing.T) {
	added, removed := diffSymbols(map[string]bool{"BTC": true, "ETH": true}, []string{"BTC", "NEW"})
	if len(added) != 1 || added[0] != "NEW" {
		t.Errorf("added = %v, want [NEW]", added)
	}
	if len(removed) != 1 || removed[0] != "ETH" {
		t.Errorf("removed = %v, want [ETH]", removed)
	}
}
