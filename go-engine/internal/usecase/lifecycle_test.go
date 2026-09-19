package usecase

import (
	"context"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"sync"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// recordingModel captures every observation it is asked about, so a test can assert on which
// lifecycle question was actually posed — the categories are the whole point of §15.12, and a
// model client that only returns actions could not distinguish "asked correctly" from "asked at
// all".
type recordingModel struct {
	mu     sync.Mutex
	obs    []domain.Observation
	action domain.Action
	err    error
}

func (m *recordingModel) Predict(ctx context.Context, obs domain.Observation) (*domain.Action, error) {
	m.mu.Lock()
	m.obs = append(m.obs, obs)
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	a := m.action
	return &a, nil
}

func (m *recordingModel) categories() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.obs))
	for i, o := range m.obs {
		out[i] = o.Category
	}
	return out
}

func (m *recordingModel) lastWithCategory(c string) (domain.Observation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.obs) - 1; i >= 0; i-- {
		if m.obs[i].Category == c {
			return m.obs[i], true
		}
	}
	return domain.Observation{}, false
}

// lifecycleTrader is a PaperTrader with the full lifecycle enabled, as a server-side training run
// would have it configured.
func lifecycleTrader(repo port.Repository, model port.ModelClient) *PaperTrader {
	pt := newTestPaperTrader(repo, nil)
	pt.Model = model
	pt.RLSizing = true
	pt.RLSLTPAdjust = true
	pt.RLEarlyClose = true
	pt.MaxLeverage = dec("100")
	pt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100"), "15m": nil}
	return pt
}

// TestClose_EmitsTerminalCall is the single most important test in this file. Before the
// conductor, PaperTrader never emitted a terminal call, so rl_service/learner.py — which pairs a
// decision with the PnL that arrives on the close call — received NO rewards in production at all.
// The model could be asked questions but never told how any answer turned out.
func TestClose_EmitsTerminalCall(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionNone}}
	pt := lifecycleTrader(repo, model)

	tp := dec("110")
	id, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), Variant: "baseline",
	})
	if err != nil {
		t.Fatalf("open order: %v", err)
	}

	// Price touches the take-profit.
	if err := pt.monitorOpenOrders(context.Background(), dec("110"), testLogger()); err != nil {
		t.Fatalf("monitor: %v", err)
	}

	obs, ok := model.lastWithCategory(domain.CategoryClosedTP)
	if !ok {
		t.Fatalf("no closed_tp call was made; categories seen: %v", model.categories())
	}
	if obs.OrderID != id {
		t.Errorf("terminal call OrderID = %d, want %d — the learner pairs the reward to the decision by this id", obs.OrderID, id)
	}
	// PnL is the reward. A terminal call carrying zero would train the model that a winning trade
	// was worth nothing.
	if !obs.PositionState.RealizedPnLUSD.IsPositive() {
		t.Errorf("terminal RealizedPnLUSD = %v, want positive for a TP hit", obs.PositionState.RealizedPnLUSD)
	}
	// The outcome must stay attached to the decision that produced it (§15.10): entry and levels
	// are deliberately NOT zeroed, or the model cannot learn which stop placement caused which
	// result.
	if !obs.PositionState.PositionOpen {
		t.Error("terminal PositionState should still describe the position, not a zeroed one")
	}
}

func TestClose_TerminalCategoryMatchesReason(t *testing.T) {
	for _, tc := range []struct {
		name     string
		slPx     string
		price    string
		wantCat  string
		wantSign int
	}{
		{"stop loss", "95", "95", domain.CategoryClosedSL, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepository()
			model := &recordingModel{action: domain.Action{Action: domain.ActionNone}}
			pt := lifecycleTrader(repo, model)

			sl := dec(tc.slPx)
			if _, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
				InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl,
				Size: dec("100"), Leverage: dec("1"), Variant: "baseline",
			}); err != nil {
				t.Fatalf("open order: %v", err)
			}

			if err := pt.monitorOpenOrders(context.Background(), dec(tc.price), testLogger()); err != nil {
				t.Fatalf("monitor: %v", err)
			}

			obs, ok := model.lastWithCategory(tc.wantCat)
			if !ok {
				t.Fatalf("no %s call; categories seen: %v", tc.wantCat, model.categories())
			}
			if obs.PositionState.RealizedPnLUSD.Sign() != tc.wantSign {
				t.Errorf("RealizedPnLUSD sign = %d, want %d", obs.PositionState.RealizedPnLUSD.Sign(), tc.wantSign)
			}
		})
	}
}

func TestClose_TerminalFailureDoesNotBlockTheClose(t *testing.T) {
	repo := newFakeRepository()
	// A model that always errors: the reward is lost for this trade, which is worth a warning, but
	// the position must still close. Leaving a position open in the database that the price feed
	// has already resolved would be far worse than an unscored decision.
	model := &recordingModel{err: context.DeadlineExceeded}
	pt := lifecycleTrader(repo, model)

	tp := dec("110")
	id, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), Variant: "baseline",
	})
	if err != nil {
		t.Fatalf("open order: %v", err)
	}

	if err := pt.monitorOpenOrders(context.Background(), dec("110"), testLogger()); err != nil {
		t.Fatalf("monitor should not surface a model error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 0 {
		t.Fatalf("order %d should be closed despite the model erroring, still open: %d", id, len(open))
	}
}

// A manual close (the panel's Close button, 2026-08-31) DOES produce a terminal call — reported
// as closed_early, the same "decision-driven exit, trains something rather than nothing"
// treatment as a timeout close (CLAUDE.md's TerminalCategory doc comment). This is a deliberate
// revision from the original "manual closes report nothing" design: the model has no
// closed_manual category to report it under honestly either way, and reporting nothing would
// leave that trade training nothing at all.
func TestClose_ManualCloseReportsClosedEarly(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionNone}}
	pt := lifecycleTrader(repo, model)

	o := port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Size: dec("100"), Leverage: dec("1")}
	id, _ := repo.OpenPaperOrder(context.Background(), o)
	o.ID = id

	if err := pt.closeOrder(context.Background(), o, dec("105"), "manual", testLogger()); err != nil {
		t.Fatalf("close: %v", err)
	}
	cats := model.categories()
	if len(cats) != 1 || cats[0] != domain.CategoryClosedEarly {
		t.Errorf("a manual close should report closed_early, got %v", cats)
	}
}

func TestOpenDecision_SkipMeansNoOrder(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionSkip}}
	pt := lifecycleTrader(repo, model)
	pt.Strategies = []StrategyAssignment{{
		Bar: "1m", StrategyID: 7, Kind: "fake",
		Strategy: &stubStrategy{signal: strategy.Signal{
			Side: strategy.Buy, Confidence: dec("0.9"), SLPct: dec("0.02"), TPPct: dec("0.04"),
		}},
	}}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 0 {
		t.Fatalf("a skip must open no order, got %d", len(open))
	}
	// The question posed must have been buy/sell — direction comes from the strategy, never from
	// the model (§9/§16.1).
	if cats := model.categories(); len(cats) != 1 || cats[0] != domain.CategoryBuy {
		t.Errorf("categories = %v, want exactly one %q", cats, domain.CategoryBuy)
	}
}

func TestOpenDecision_AppliesModelLevelsClamped(t *testing.T) {
	repo := newFakeRepository()
	// The model asks for a stop 0.1% away — noise-level, would stop out instantly. The clamp
	// widens it to the 0.5% floor before it is ever written to the order.
	model := &recordingModel{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.1"), LeverageFrac: dec("0.1"),
		SLPx: dec("99.9"), TPPx: dec("104"),
	}}
	pt := lifecycleTrader(repo, model)
	pt.RLClamps = conductor.Clamps{MinSLDistPct: dec("0.005"), MaxSLDistPct: dec("0.05"), MinTPSLRatio: dec("1.5")}
	pt.Strategies = []StrategyAssignment{{
		Bar: "1m", StrategyID: 7, Kind: "fake",
		Strategy: &stubStrategy{signal: strategy.Signal{
			Side: strategy.Buy, Confidence: dec("0.9"), SLPct: dec("0.02"), TPPct: dec("0.04"),
		}},
	}}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected 1 open order, got %d", len(open))
	}
	if open[0].SLPx == nil || !open[0].SLPx.Equal(dec("99.5")) {
		t.Errorf("SL = %v, want the model's level clamped to the 0.5%% floor (99.5)", open[0].SLPx)
	}
}

func TestOpenDecision_KeepsStrategyLevelsWhenModelSetsNone(t *testing.T) {
	repo := newFakeRepository()
	// The model sized the trade but set no levels. Opening with no protection because it stayed
	// silent would be strictly worse than the structure-derived stop the strategy already proposed.
	model := &recordingModel{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.1"), LeverageFrac: dec("0.1")}}
	pt := lifecycleTrader(repo, model)
	pt.Strategies = []StrategyAssignment{{
		Bar: "1m", StrategyID: 7, Kind: "fake",
		Strategy: &stubStrategy{signal: strategy.Signal{
			Side: strategy.Buy, Confidence: dec("0.9"), SLPct: dec("0.02"), TPPct: dec("0.04"),
		}},
	}}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected 1 open order, got %d", len(open))
	}
	if open[0].SLPx == nil || !open[0].SLPx.Equal(dec("98")) {
		t.Errorf("SL = %v, want the strategy's own 2%% stop (98)", open[0].SLPx)
	}
}

func TestUpdate_EarlyCloseRecordsRLEarly(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionClose}}
	pt := lifecycleTrader(repo, model)

	sl, tp := dec("95"), dec("110")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), Variant: "baseline",
	})

	// The first call only establishes the update cadence baseline (CLAUDE.md's 2026-09-03 fix —
	// an order's first-ever update check no longer fires on sight, since that was closing brand
	// new positions within ~1s of opening once early-close could act on the answer). The second
	// call, once a real PnL move separates it from the baseline, is what actually reaches the model.
	pt.runUpdates(context.Background(), "1m", dec("100"), testLogger())
	// Price is between the levels — nothing would close this except the model's own decision.
	pt.runUpdates(context.Background(), "1m", dec("103"), testLogger())

	closed := repo.orders[id]
	if closed.ClosedAt == nil {
		t.Fatal("model asked to close but the order is still open")
	}
	if closed.CloseReason == nil || *closed.CloseReason != conductor.CloseReasonRLEarly {
		t.Errorf("close_reason = %v, want %q — an early close must stay distinguishable from operator action and from SL/TP",
			closed.CloseReason, conductor.CloseReasonRLEarly)
	}
	if _, ok := model.lastWithCategory(domain.CategoryClosedEarly); !ok {
		t.Errorf("an early close must still deliver its reward; categories seen: %v", model.categories())
	}
}

func TestUpdate_EarlyCloseIgnoredWhenDisabled(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionClose}}
	pt := lifecycleTrader(repo, model)
	pt.RLEarlyClose = false // the default: early close destroys the counterfactual, so it is opt-in

	sl, tp := dec("95"), dec("110")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), Variant: "baseline",
	})

	pt.runUpdates(context.Background(), "1m", dec("103"), testLogger())

	if repo.orders[id].ClosedAt != nil {
		t.Error("early close must be ignored while RLEarlyClose is off")
	}
}

func TestUpdate_NoForkOfAFork(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionUpdate, SLPx: dec("99"), TPPx: dec("108")}}
	pt := lifecycleTrader(repo, model)

	// An rl_adjusted fork is an open position and does get its own update calls, but forking it
	// again would grow a tree without bound, each leaf drawing model calls forever.
	sl, tp := dec("95"), dec("110")
	parent := int64(999)
	if _, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), Variant: "rl_adjusted", ParentOrderID: &parent,
	}); err != nil {
		t.Fatalf("open fork: %v", err)
	}

	pt.runUpdates(context.Background(), "1m", dec("100"), testLogger()) // establishes the baseline
	pt.runUpdates(context.Background(), "1m", dec("103"), testLogger())

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Errorf("a fork must not be forked again, got %d open orders", len(open))
	}
	// It should still have been ASKED, though — forks are positions the model manages too.
	if len(model.categories()) == 0 {
		t.Error("a fork should still receive update calls")
	}
}

func TestUpdate_CadenceSuppressesRedundantCalls(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionNone}}
	pt := lifecycleTrader(repo, model)
	pt.RLUpdatePnLThresholdPct = dec("0.01")
	pt.RLUpdateMaxInterval = time.Hour

	sl, tp := dec("90"), dec("120")
	if _, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), Variant: "baseline",
	}); err != nil {
		t.Fatalf("open order: %v", err)
	}

	// The first call only establishes the cadence baseline (CLAUDE.md's 2026-09-03 fix) and must
	// not itself reach the model.
	pt.runUpdates(context.Background(), "1m", dec("100"), testLogger())
	first := len(model.categories())
	if first != 0 {
		t.Fatalf("an order's first update check should only establish the baseline, got %d calls", first)
	}

	// A 0.2% move is below the 1% threshold — the whole point of PnL-delta cadence is that a
	// position ranging quietly does not generate thousands of near-identical training steps.
	pt.runUpdates(context.Background(), "1m", dec("100.2"), testLogger())
	if got := len(model.categories()); got != first {
		t.Errorf("sub-threshold move produced %d calls, want no additional call", got-first)
	}

	// A 2% move is real and must reach the model.
	pt.runUpdates(context.Background(), "1m", dec("102"), testLogger())
	if got := len(model.categories()); got != first+1 {
		t.Errorf("a 2%% move should fire exactly one update, total calls = %d", got)
	}
}

func TestUpdate_CarriesSignalForward(t *testing.T) {
	repo := newFakeRepository()
	model := &recordingModel{action: domain.Action{Action: domain.ActionNone}}
	pt := lifecycleTrader(repo, model)
	pt.RLSizing = false // isolate the update path from the open decision
	pt.Strategies = []StrategyAssignment{{
		Bar: "1m", StrategyID: 7, Kind: "rsi_sma",
		Strategy: &stubStrategy{signal: strategy.Signal{
			Side: strategy.Buy, Confidence: dec("0.9"), SLPct: dec("0.02"), TPPct: dec("0.04"),
		}},
	}}

	// A strategy fires on its candle close, opening a position.
	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// The first update call establishes the cadence baseline (CLAUDE.md's 2026-09-03 fix) and
	// does not itself reach the model.
	pt.runUpdates(context.Background(), "1m", dec("100"), testLogger())

	// Later, a price-driven update runs with no strategy firing. The retained signal must still be
	// attached: a higher-timeframe opinion stays meaningful between its candles, and dropping it
	// would hide it from every update in between (§15.12).
	pt.runUpdates(context.Background(), "1m", dec("101"), testLogger())

	obs, ok := model.lastWithCategory(domain.CategoryUpdate)
	if !ok {
		t.Fatalf("no update call; categories seen: %v", model.categories())
	}
	if obs.Signal == nil {
		t.Fatal("update call carried no signal; the last 1m signal should have been carried forward")
	}
	if obs.Signal.Kind != "rsi_sma" || obs.Signal.Side != "buy" {
		t.Errorf("carried signal = %+v, want the rsi_sma buy that opened the position", obs.Signal)
	}
}

// TestCloseEarly_IgnoredRequestIsRecorded covers the observability gap found on 2026-09-10: paper
// trading's closeEarly returned on the disabled flag with no log and no metric, while BotTrader's
// equivalent had recorded both since 2026-09-08.
//
// The silence is what made a real symptom unexplainable. SL/TP adjustments looked broken (690/day
// down to 1/day) and the actual cause was that the model had converged to answering "close" on 2150
// of 2174 update calls, with "update" — the answer that adjusts SL/TP — down to 2. The adjustment
// path worked the whole time; it was just no longer the answer being given, and nothing recorded
// the discarded closes.
func TestCloseEarly_IgnoredRequestIsRecorded(t *testing.T) {
	repo := newFakeRepository()
	pt := &PaperTrader{
		InstID:       "BTC",
		Repo:         repo,
		RLEarlyClose: false, // the production setting that suppresses the action
	}
	o := port.PaperOrder{ID: 1, InstID: "BTC", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.orders[1] = o

	before := testutil.ToFloat64(metrics.PaperEarlyCloseIgnoredTotal.WithLabelValues("BTC"))
	pt.closeEarly(context.Background(), o, dec("101"), testLogger())
	after := testutil.ToFloat64(metrics.PaperEarlyCloseIgnoredTotal.WithLabelValues("BTC"))

	if after != before+1 {
		t.Errorf("expected the suppressed early-close to be counted, got %v -> %v", before, after)
	}
	if repo.orders[1].ClosedAt != nil {
		t.Error("the position must NOT be closed while rl_early_close is off; only the request is recorded")
	}
}

// TestCloseEarly_EnabledStillCloses guards the other direction: recording the suppressed case must
// not accidentally suppress the enabled one too.
func TestCloseEarly_EnabledStillCloses(t *testing.T) {
	repo := newFakeRepository()
	pt := &PaperTrader{
		InstID:       "BTC",
		Repo:         repo,
		RLEarlyClose: true,
	}
	o := port.PaperOrder{ID: 1, InstID: "BTC", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.orders[1] = o

	pt.closeEarly(context.Background(), o, dec("101"), testLogger())

	if repo.orders[1].ClosedAt == nil {
		t.Error("expected the position to be closed when rl_early_close is on")
	}
}
