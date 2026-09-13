package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// This file is the database-backed instrument roster's read side — what every trading service loads
// instead of config.yaml's trading.inst_ids plus its hand-maintained trading.symbol_map
// (2026-09-13, migration 000031).
//
// It exists so a token the discovery scan admitted actually reaches the WebSocket subscriptions. The
// scan could already write a roster row; nothing could read one.

// Roster is one service's resolved working set: which short symbols to work with, and what each one
// resolves to on the exchange.
type Roster struct {
	// Symbols are the short internal identities ("BTC") every other table, Kafka key and panel row
	// carries. Sorted, so a service's startup log and a restart comparison are stable.
	Symbols []string
	// ExecInstID maps each symbol to the exchange's own wire-format instrument id. This replaces
	// trading.symbol_map, and is the reason the roster had to move to the database at all: a scanned
	// token has no YAML entry, so under the old arrangement it could not be subscribed to.
	ExecInstID map[string]string
	// InstType per symbol, since an exchange can carry the same token under more than one product
	// family (OKX lists both classic SWAP perpetuals and the X-Perp FUTURES ones, §33.2).
	InstType map[string]string
	// Exchange per symbol, for a future multi-exchange ingestor. Today every consumer runs
	// single-exchange and filters by it.
	Exchange map[string]string
}

// RosterFor reads one consumer's enabled instruments. consumer is "ingest", "paper" or "real" — the
// three independent flags migration 000031 keeps separate precisely so a discovered token can
// collect data and paper-trade while staying off for real money.
//
// FALLING BACK TO CONFIG IS DELIBERATE AND BOUNDED: an EMPTY roster (no rows at all for this
// exchange) means the table has never been seeded, which is the state of every deployment the moment
// this migration lands — so config.yaml's own list is used and seeded, and nothing breaks on the
// deploy. A roster that exists but has every row DISABLED is NOT the same thing and is honored
// as-is: falling back there would resurrect tokens an operator deliberately turned off, which is
// migration 000030's exact bug.
func RosterFor(
	ctx context.Context,
	repo port.Repository,
	exchange, consumer string,
	fallbackSymbols []string,
	fallbackExecIDs map[string]string,
	fallbackInstType string,
	logger *slog.Logger,
) (Roster, error) {
	all, err := repo.ListInstruments(ctx, port.InstrumentFilter{Exchange: exchange})
	if err != nil {
		return Roster{}, fmt.Errorf("load roster: %w", err)
	}
	if len(all) == 0 && len(fallbackSymbols) > 0 {
		if logger != nil {
			logger.Info("instrument roster is empty, seeding it from config",
				"exchange", exchange, "symbols", len(fallbackSymbols))
		}
		if err := SeedRoster(ctx, repo, exchange, fallbackSymbols, fallbackExecIDs, fallbackInstType); err != nil {
			return Roster{}, err
		}
		all, err = repo.ListInstruments(ctx, port.InstrumentFilter{Exchange: exchange})
		if err != nil {
			return Roster{}, fmt.Errorf("load roster after seeding: %w", err)
		}
	}

	r := Roster{
		ExecInstID: map[string]string{},
		InstType:   map[string]string{},
		Exchange:   map[string]string{},
	}
	for _, in := range all {
		enabled, err := enabledFor(in, consumer)
		if err != nil {
			return Roster{}, err
		}
		if !enabled {
			continue
		}
		// A row with no exec id cannot be subscribed to or traded. Skipped loudly rather than
		// subscribed to as an empty string, which OKX accepts and then silently pushes nothing for —
		// the same "loud failure over a data gap that looks healthy" rule as §9's bar-name casing.
		if in.ExecInstID == "" {
			if logger != nil {
				logger.Error("roster row has no exec_inst_id, skipping",
					"exchange", in.Exchange, "symbol", in.Symbol)
			}
			continue
		}
		r.Symbols = append(r.Symbols, in.Symbol)
		r.ExecInstID[in.Symbol] = in.ExecInstID
		r.InstType[in.Symbol] = in.InstType
		r.Exchange[in.Symbol] = in.Exchange
	}
	sort.Strings(r.Symbols)
	return r, nil
}

// enabledFor maps a consumer name onto its own flag. An unknown name is an error rather than a
// silent false: a typo would otherwise read as "this service has no instruments", which looks like a
// quiet market rather than a wiring mistake.
func enabledFor(in port.Instrument, consumer string) (bool, error) {
	switch consumer {
	case "ingest":
		return in.EnabledIngest, nil
	case "paper":
		return in.EnabledPaper, nil
	case "real":
		return in.EnabledReal, nil
	default:
		return false, fmt.Errorf("load roster: unknown consumer %q", consumer)
	}
}

// SeedRoster writes config.yaml's own instrument list into the roster table, for the one-time
// transition on the deploy that introduces it.
//
// source="seed" rather than "manual" so a reader can always tell a transitional row from one a
// person actually chose (the same provenance reasoning as migration 000030's
// auto_disabled_inst_ids). Real mode is enabled for a seeded token, deliberately UNLIKE a scanned
// one: these are the tokens real trading was already configured for, and turning them all off during
// a migration would be a silent behavior change dressed up as a data move.
func SeedRoster(
	ctx context.Context,
	repo port.Repository,
	exchange string,
	symbols []string,
	execIDs map[string]string,
	instType string,
) error {
	for _, sym := range symbols {
		execID := execIDs[sym]
		if execID == "" {
			// Refuse rather than seed an unusable row: config validation already requires a
			// symbol_map entry for every configured symbol, so reaching here means the two disagree,
			// and inventing an id would subscribe to nothing.
			return fmt.Errorf("seed roster: symbol %q has no exec instId in config", sym)
		}
		in := port.Instrument{
			Symbol: sym, Exchange: exchange, ExecInstID: execID, InstType: instType,
			EnabledIngest: true, EnabledPaper: true, EnabledReal: true,
			Source: "seed",
		}
		if _, err := repo.UpsertInstrument(ctx, in); err != nil {
			return fmt.Errorf("seed roster: %s: %w", sym, err)
		}
	}
	return nil
}

// SeedExecIDs resolves config.yaml's symbol list against its symbol_map, for the ONE-TIME seed of a
// database that has never held a roster. It fails loudly on a missing entry rather than seeding a row
// with an empty exec id, which OKX accepts as a subscription and then silently pushes nothing for.
//
// Takes a plain map rather than an okx.SymbolMap so this package stays free of any adapter import —
// the layering rule TestUsecaseImportsNoExchangeAdapter enforces (§46.1). After the seed, that config
// map is never consulted again: the roster's own exec_inst_id column replaces it, which is precisely
// what makes a scan-discovered token subscribable.
func SeedExecIDs(symbolMap map[string]string, symbols []string) (map[string]string, error) {
	out := make(map[string]string, len(symbols))
	for _, sym := range symbols {
		id := symbolMap[sym]
		if id == "" {
			return nil, fmt.Errorf("symbol %q has no symbol_map entry", sym)
		}
		out[sym] = id
	}
	return out, nil
}

// RosterWatcher notices when the roster has changed underneath a running service and calls OnChange.
//
// This is what closes the loop on "a scanned token should reach the WebSockets": the scan writes a
// row, and the ingestor's subscriptions are resolved once at startup (each is a live socket, and
// OKX's protocol offers no way to extend an established subscription mid-flight). Rather than teach
// every service to mutate its own sockets, the watcher has the process exit — the same
// self-exit-plus-restart-policy mechanism cmd/strategy-tester (§18) and cmd/paper-trader (§22)
// already use for config changes, chosen there for the same reason: a restart re-derives every piece
// of state from the database, where a partial in-place reload is exactly how a service ends up
// half-subscribed with nothing reporting it.
type RosterWatcher struct {
	Repo     port.Repository
	Exchange string
	Consumer string // "ingest", "paper" or "real"
	Interval time.Duration
	Logger   *slog.Logger
	// OnChange is called once, with the reason, when the enabled set differs from Baseline. The
	// caller decides what to do (in practice: log and exit non-zero so the restart policy relaunches
	// it reading the new roster).
	OnChange func(reason string)
	// Baseline is the symbol set the service actually started with.
	Baseline []string
}

// Run polls until ctx is done or a change is seen. It returns after calling OnChange once: a service
// about to restart has nothing further to watch for, and continuing to poll would fire the callback
// repeatedly through the shutdown window.
func (w *RosterWatcher) Run(ctx context.Context) {
	if w.Interval <= 0 || w.OnChange == nil {
		return
	}
	baseline := make(map[string]bool, len(w.Baseline))
	for _, s := range w.Baseline {
		baseline[s] = true
	}

	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		// No fallback symbols: a watcher must never seed. Seeding is a startup decision made once,
		// and doing it from here would let a transient empty read rewrite the roster.
		current, err := RosterFor(ctx, w.Repo, w.Exchange, w.Consumer, nil, nil, "", w.Logger)
		if err != nil {
			// A database blip must not restart a healthy trading service. Log and try again next
			// tick — the roster changes on the order of days, so there is no urgency that would
			// justify acting on an unreadable answer.
			if w.Logger != nil {
				w.Logger.Warn("roster watcher: could not read roster, will retry", "error", err)
			}
			continue
		}
		// An empty result is treated as unreadable, not as "every token was removed": RosterFor
		// seeds from config only when the TABLE is empty, so an empty answer here means every row is
		// disabled or unusable, and restarting into an empty roster would just crash-loop.
		if len(current.Symbols) == 0 {
			if w.Logger != nil {
				w.Logger.Warn("roster watcher: roster is empty, not restarting")
			}
			continue
		}

		if added, removed := diffSymbols(baseline, current.Symbols); len(added)+len(removed) > 0 {
			w.OnChange(fmt.Sprintf("roster changed (added=%v removed=%v)", added, removed))
			return
		}
	}
}

// diffSymbols reports what the current set has that the baseline does not, and vice versa.
func diffSymbols(baseline map[string]bool, current []string) (added, removed []string) {
	seen := make(map[string]bool, len(current))
	for _, s := range current {
		seen[s] = true
		if !baseline[s] {
			added = append(added, s)
		}
	}
	for s := range baseline {
		if !seen[s] {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
