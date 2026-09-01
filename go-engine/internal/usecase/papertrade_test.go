package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// fakeRepository is an in-memory port.Repository for testing, no real Postgres needed. Guarded
// by a mutex since PaperTrader runs one goroutine per bar consumer plus one for ticks, all of
// which can call into the repository concurrently (real Postgres handles this natively; this
// fake must emulate that instead of assuming single-goroutine test access).
type fakeRepository struct {
	mu                 sync.Mutex
	nextID             int64
	orders             map[int64]port.PaperOrder
	candles            []port.Candle
	accounts           map[string]port.AccountEquity
	equityPoints       []port.EquityPoint
	paramChanges       []port.ParamChange
	paperTradingConfig *port.PaperTradingConfig
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{orders: make(map[int64]port.PaperOrder), accounts: make(map[string]port.AccountEquity)}
}

func (r *fakeRepository) SaveCandle(ctx context.Context, c port.Candle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Upsert on (inst_id, bar, ts), mirroring the real Postgres implementation's ON CONFLICT.
	// A plain append would let a test see duplicate rows that production cannot produce — and
	// would make the backfill's idempotency (which is what allows an interrupted run to simply be
	// re-run) untestable against this fake.
	for i, existing := range r.candles {
		if existing.InstID == c.InstID && existing.Bar == c.Bar && existing.Timestamp.Equal(c.Timestamp) {
			r.candles[i] = c
			return nil
		}
	}
	r.candles = append(r.candles, c)
	return nil
}
func (r *fakeRepository) ListCandles(ctx context.Context, instID, bar string, limit int) ([]port.Candle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.Candle
	for _, c := range r.candles {
		if c.InstID == instID && c.Bar == bar {
			out = append(out, c)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
func (r *fakeRepository) CreateStrategy(ctx context.Context, s port.StrategyConfig) (int64, error) {
	return 0, nil
}
func (r *fakeRepository) GetStrategy(ctx context.Context, id int64) (port.StrategyConfig, error) {
	return port.StrategyConfig{}, nil
}
func (r *fakeRepository) ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]port.StrategyConfig, error) {
	return nil, nil
}
func (r *fakeRepository) UpdateStrategyConfig(ctx context.Context, id int64, config json.RawMessage, enabled bool) error {
	return nil
}
func (r *fakeRepository) DeleteStrategy(ctx context.Context, id int64) error        { return nil }
func (r *fakeRepository) ResetStrategyToOrigin(ctx context.Context, id int64) error { return nil }
func (r *fakeRepository) CreateAssignment(ctx context.Context, a port.StrategyAssignment) (int64, error) {
	return 0, nil
}
func (r *fakeRepository) ListAssignments(ctx context.Context, instID string, enabledOnly bool) ([]port.StrategyAssignment, error) {
	return nil, nil
}
func (r *fakeRepository) SetAssignmentEnabled(ctx context.Context, id int64, enabled bool) error {
	return nil
}
func (r *fakeRepository) DeleteAssignment(ctx context.Context, id int64) error { return nil }
func (r *fakeRepository) StrategyStatsFor(ctx context.Context, strategyID int64) (port.StrategyStats, error) {
	return port.StrategyStats{}, nil
}
func (r *fakeRepository) ListPositions(ctx context.Context, f port.PositionFilter) ([]port.PaperOrder, error) {
	return nil, nil
}
func (r *fakeRepository) OpenPaperOrder(ctx context.Context, o port.PaperOrder) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	o.ID = r.nextID
	r.orders[o.ID] = o
	return o.ID, nil
}
func (r *fakeRepository) ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.orders[id]
	now := o.OpenedAt
	o.ClosedAt = &now
	o.CloseReason = &reason
	cp := closePx
	o.ClosePx = &cp
	pnl := realizedPnL
	o.RealizedPnL = &pnl
	r.orders[id] = o
	return nil
}
func (r *fakeRepository) RequestManualClose(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok || o.ClosedAt != nil {
		return fmt.Errorf("order %d is not open", id)
	}
	o.ManualCloseRequested = true
	r.orders[id] = o
	return nil
}
func (r *fakeRepository) RequestManualCloseAll(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, o := range r.orders {
		if o.ClosedAt == nil {
			o.ManualCloseRequested = true
			r.orders[id] = o
			n++
		}
	}
	return n, nil
}
func (r *fakeRepository) GetPaperTradingConfig(ctx context.Context) (port.PaperTradingConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paperTradingConfig == nil {
		return port.PaperTradingConfig{TradingState: "running"}, nil
	}
	return *r.paperTradingConfig, nil
}
func (r *fakeRepository) SavePaperTradingConfig(ctx context.Context, patch port.PaperTradingConfigPatch) (port.PaperTradingConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := port.PaperTradingConfig{TradingState: "running"}
	if r.paperTradingConfig != nil {
		c = *r.paperTradingConfig
	}
	if patch.TradingState != nil {
		c.TradingState = *patch.TradingState
	}
	if patch.DisableLong != nil {
		c.DisableLong = *patch.DisableLong
	}
	if patch.DisableShort != nil {
		c.DisableShort = *patch.DisableShort
	}
	if patch.ActiveKinds != nil {
		c.ActiveKinds = *patch.ActiveKinds
	}
	if patch.DisabledInstIDs != nil {
		c.DisabledInstIDs = *patch.DisabledInstIDs
	}
	if patch.ActiveBars != nil {
		c.ActiveBars = *patch.ActiveBars
	}
	r.paperTradingConfig = &c
	return c, nil
}
func (r *fakeRepository) SetAssignmentsEnabledForKinds(ctx context.Context, activeKinds []string) error {
	return nil
}
func (r *fakeRepository) UpdatePaperOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok || o.ClosedAt != nil {
		return nil
	}
	o.SLPx = slPx
	o.TPPx = tpPx
	r.orders[id] = o
	return nil
}
func (r *fakeRepository) ForkPaperOrderWithSLTP(ctx context.Context, parentID int64, slPx, tpPx *decimal.Decimal) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parent, ok := r.orders[parentID]
	if !ok || parent.ClosedAt != nil {
		return 0, fmt.Errorf("fork: parent order %d not open", parentID)
	}
	r.nextID++
	fork := parent
	fork.ID = r.nextID
	fork.SLPx = slPx
	fork.TPPx = tpPx
	fork.ParentOrderID = &parentID
	fork.Variant = "rl_adjusted"
	r.orders[fork.ID] = fork
	return fork.ID, nil
}
func (r *fakeRepository) UpdatePaperOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok || o.ClosedAt != nil {
		return nil
	}
	if maxPct.GreaterThan(o.PnLMaxPct) {
		o.PnLMaxPct = maxPct
	}
	if minPct.LessThan(o.PnLMinPct) {
		o.PnLMinPct = minPct
	}
	r.orders[id] = o
	return nil
}

func (r *fakeRepository) GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ae, ok := r.accounts[mode]; ok {
		return ae, nil
	}
	ae := port.AccountEquity{Mode: mode, InitialUSD: initialUSD, EquityUSD: initialUSD}
	r.accounts[mode] = ae
	r.equityPoints = append(r.equityPoints, port.EquityPoint{Mode: mode, EquityUSD: initialUSD, Reason: "seed"})
	return ae, nil
}

func (r *fakeRepository) ApplyRealizedPnL(ctx context.Context, mode string, pnl decimal.Decimal, orderID *int64, instID string) (port.AccountEquity, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ae, ok := r.accounts[mode]
	if !ok {
		return port.AccountEquity{}, false, fmt.Errorf("apply pnl: no account row for mode %s", mode)
	}
	ae.EquityUSD = ae.EquityUSD.Add(pnl)
	r.equityPoints = append(r.equityPoints, port.EquityPoint{
		Mode: mode, EquityUSD: ae.EquityUSD, DeltaUSD: pnl, Reason: "trade", OrderID: orderID, InstID: instID,
	})

	reset := false
	// Real mode never auto-resets a drained balance (CLAUDE.md §15.7) — mirrored here so tests
	// exercise the same carve-out the Postgres implementation enforces.
	if ae.EquityUSD.Sign() <= 0 && mode != "real" {
		drained := ae.EquityUSD
		ae.EquityUSD = ae.InitialUSD
		ae.ResetCount++
		reset = true
		r.equityPoints = append(r.equityPoints, port.EquityPoint{
			Mode: mode, EquityUSD: ae.EquityUSD, DeltaUSD: ae.EquityUSD.Sub(drained), Reason: "reset", InstID: instID,
		})
	}
	r.accounts[mode] = ae
	return ae, reset, nil
}

func (r *fakeRepository) ListEquityHistory(ctx context.Context, mode string, since time.Time, limit int) ([]port.EquityPoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.EquityPoint
	for _, p := range r.equityPoints {
		if p.Mode == mode {
			out = append(out, p)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (r *fakeRepository) SLTPAdjustmentStats(ctx context.Context, instID string, since time.Time) ([]port.VariantStats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	adjustedBaselineIDs := make(map[int64]bool)
	for _, o := range r.orders {
		if o.Variant == "rl_adjusted" && o.ParentOrderID != nil {
			if b, ok := r.orders[*o.ParentOrderID]; ok && (instID == "" || b.InstID == instID) && !b.OpenedAt.Before(since) {
				adjustedBaselineIDs[*o.ParentOrderID] = true
			}
		}
	}

	baseline := port.VariantStats{Variant: "baseline", RealizedPnL: decimal.Zero}
	adjusted := port.VariantStats{Variant: "rl_adjusted", RealizedPnL: decimal.Zero}
	for _, o := range r.orders {
		if o.Variant == "baseline" || o.Variant == "" {
			if !adjustedBaselineIDs[o.ID] {
				continue
			}
			accumulateVariantStats(&baseline, o)
		} else if o.Variant == "rl_adjusted" && o.ParentOrderID != nil && adjustedBaselineIDs[*o.ParentOrderID] {
			accumulateVariantStats(&adjusted, o)
		}
	}
	return []port.VariantStats{baseline, adjusted}, nil
}
func accumulateVariantStats(vs *port.VariantStats, o port.PaperOrder) {
	if o.ClosedAt == nil {
		return
	}
	vs.ClosedCount++
	if o.CloseReason != nil && *o.CloseReason == "tp" {
		vs.Wins++
	}
	if o.CloseReason != nil && *o.CloseReason == "sl" {
		vs.Losses++
	}
	if o.RealizedPnL != nil {
		vs.RealizedPnL = vs.RealizedPnL.Add(*o.RealizedPnL)
	}
}
func (r *fakeRepository) ListSLTPAdjustmentPairs(ctx context.Context, instID string) ([]port.SLTPAdjustmentPair, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.SLTPAdjustmentPair
	for _, o := range r.orders {
		if o.Variant != "rl_adjusted" || o.ParentOrderID == nil {
			continue
		}
		b, ok := r.orders[*o.ParentOrderID]
		if !ok || (instID != "" && b.InstID != instID) {
			continue
		}
		out = append(out, port.SLTPAdjustmentPair{InstID: b.InstID, BaselineOrder: b, RLAdjustedOrder: o})
	}
	return out, nil
}
func (r *fakeRepository) ListOpenPaperOrders(ctx context.Context, instID string) ([]port.PaperOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.PaperOrder
	for _, o := range r.orders {
		if o.InstID == instID && o.ClosedAt == nil {
			out = append(out, o)
		}
	}
	return out, nil
}
func (r *fakeRepository) RecordParamChange(ctx context.Context, c port.ParamChange) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	c.ID = r.nextID
	r.paramChanges = append(r.paramChanges, c)
	return c.ID, nil
}
func (r *fakeRepository) ListParamChanges(ctx context.Context, instID string, since time.Time) ([]port.ParamChange, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.ParamChange
	for _, c := range r.paramChanges {
		if c.InstID == instID && !c.CreatedAt.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

// noopConsumer satisfies port.MarketDataConsumer without ever invoking the handler — sufficient
// for tests that drive PaperTrader through its handler methods directly.
type noopConsumer struct{}

func (noopConsumer) Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func newTestPaperTrader(repo port.Repository, strategies []StrategyAssignment) *PaperTrader {
	return &PaperTrader{
		InstID:       "BTC-USDT-SWAP",
		Bars:         []string{"1m", "15m"},
		CandleWindow: 100,
		Strategies:   strategies,
		TickConsumer: noopConsumer{},
		CandleConsumers: map[string]port.MarketDataConsumer{
			"1m":  noopConsumer{},
			"15m": noopConsumer{},
		},
		Repo:        repo,
		NotionalUSD: dec("100"),
		// Shared-account defaults (CLAUDE.md §15.6): a $1000 pool with the production caps, so
		// sizing tests exercise the real cap arithmetic rather than an unbounded path.
		Mode:                "paper",
		AccountInitialUSD:   dec("1000"),
		MaxPositionPct:      dec("0.25"),
		MaxTotalExposurePct: dec("0.60"),
		MaxOpenOrders:       3,
		Logger:              testLogger(),
		candles:             map[string][]domain.Candle{"1m": nil, "15m": nil},
	}
}

func TestCloseReason_SLHitOnBuy(t *testing.T) {
	sl := dec("99")
	order := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: &sl}
	reason, hit := closeReason(order, dec("99"))
	if !hit || reason != "sl" {
		t.Errorf("expected sl hit at price==SL, got hit=%v reason=%q", hit, reason)
	}
}

func TestCloseReason_TPHitOnSell(t *testing.T) {
	tp := dec("90")
	order := port.PaperOrder{Side: "sell", EntryPx: dec("100"), TPPx: &tp}
	reason, hit := closeReason(order, dec("90"))
	if !hit || reason != "tp" {
		t.Errorf("expected tp hit at price==TP for a sell, got hit=%v reason=%q", hit, reason)
	}
}

func TestCloseReason_PriceBetweenSLAndTPDoesNotClose(t *testing.T) {
	sl, tp := dec("99"), dec("102")
	order := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp}
	_, hit := closeReason(order, dec("100.5"))
	if hit {
		t.Errorf("expected no close for a price strictly between SL and TP")
	}
}

func TestRealizedPnL_ExactNoFloatDrift(t *testing.T) {
	// Regression test for the float64->decimal.Decimal migration.
	order := port.PaperOrder{Side: "buy", EntryPx: dec("9.0"), Size: dec("100"), Leverage: dec("5")}
	pnl := realizedPnL(order, dec("9.0")) // exact round-trip close, PnL must be exactly zero
	if !pnl.IsZero() {
		t.Errorf("expected exact zero PnL for a round-trip close, got %s", pnl.String())
	}

	pnlProfit := realizedPnL(order, dec("9.9")) // +10% move
	want := dec("50")                           // (9.9-9.0)/9.0 * 100 * 5 = 0.1 * 500 = 50
	if !pnlProfit.Equal(want) {
		t.Errorf("expected exact PnL %s, got %s", want, pnlProfit.String())
	}
}

func TestBuildPaperOrder_SLTPForBuyAndSell(t *testing.T) {
	buySignal := strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}
	buyOrder := buildPaperOrder("BTC-USDT-SWAP", dec("100"), buySignal, dec("100"), 0, "5m")
	if buyOrder.SLPx == nil || !buyOrder.SLPx.Equal(dec("99")) {
		t.Errorf("expected buy SL=99, got %v", buyOrder.SLPx)
	}
	if buyOrder.TPPx == nil || !buyOrder.TPPx.Equal(dec("102")) {
		t.Errorf("expected buy TP=102, got %v", buyOrder.TPPx)
	}

	sellSignal := strategy.Signal{Side: strategy.Sell, SLPct: dec("0.01"), TPPct: dec("0.02")}
	sellOrder := buildPaperOrder("BTC-USDT-SWAP", dec("100"), sellSignal, dec("100"), 0, "5m")
	if sellOrder.SLPx == nil || !sellOrder.SLPx.Equal(dec("101")) {
		t.Errorf("expected sell SL=101, got %v", sellOrder.SLPx)
	}
	if sellOrder.TPPx == nil || !sellOrder.TPPx.Equal(dec("98")) {
		t.Errorf("expected sell TP=98, got %v", sellOrder.TPPx)
	}
}

func TestMaxOpenOrders_GatesNewSignals(t *testing.T) {
	repo := newFakeRepository()
	// Pre-fill 3 open orders (= MaxOpenOrders) for this instrument.
	for i := 0; i < 3; i++ {
		_, _ = repo.OpenPaperOrder(context.Background(), port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Size: dec("100"), Leverage: dec("1")})
	}

	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "1m", Strategy: alwaysBuy}})
	pt.candles["1m"] = []domain.Candle{{Open: dec("100"), High: dec("101"), Low: dec("99"), Close: dec("100"), Volume: dec("1")}}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 3 {
		t.Errorf("expected MaxOpenOrders to gate new signals, still expected 3 open orders, got %d", len(open))
	}
}

func TestEvaluateStrategies_OnlyRunsStrategyAssignedToThatBar(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy1m := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	alwaysBuy15m := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{
		{Bar: "1m", Strategy: alwaysBuy1m},
		{Bar: "15m", Strategy: alwaysBuy15m},
	})
	pt.candles["1m"] = []domain.Candle{{Close: dec("100")}}

	// A 1m candle close should only trigger the 1m-assigned strategy, not the 15m one.
	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected exactly 1 order opened (from the 1m strategy only), got %d", len(open))
	}
}

// After a restart the in-memory candle window must come back from the database rather than
// re-accumulating from the live feed. Observed 2026-08-29: with empty windows, a 1H bar needs ~2
// days of live candles to fill a 50-candle window, so 12 of 14 strategies returned hold and every
// open position came from the one strategy needing the fewest candles (grid_like on 5m). The
// candles were already in Postgres the whole time — nothing was reading them.
func TestSeedCandlesFromRepo_FillsWindowFromDatabase(t *testing.T) {
	repo := newFakeRepository()
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		_ = repo.SaveCandle(ctx, port.Candle{
			InstID: "BTC-USDT-SWAP", Bar: "1H",
			Candle: domain.Candle{
				Timestamp: time.Unix(int64(i)*3600, 0),
				Open:      dec("100"), High: dec("101"), Low: dec("99"), Close: dec("100"), Volume: dec("1"),
			},
		})
	}

	pt := newTestPaperTrader(repo, nil)
	pt.Bars = []string{"1H"}
	pt.CandleWindow = 50
	pt.candles = make(map[string][]domain.Candle)
	pt.seedCandlesFromRepo(ctx, testLogger())

	if got := len(pt.candles["1H"]); got != 30 {
		t.Fatalf("expected the 30 persisted candles to seed the window, got %d", got)
	}
}

// The window must respect CandleWindow, so seeding can't hand handleCandle more than it keeps.
func TestSeedCandlesFromRepo_RespectsWindowLimit(t *testing.T) {
	repo := newFakeRepository()
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		_ = repo.SaveCandle(ctx, port.Candle{
			InstID: "BTC-USDT-SWAP", Bar: "5m",
			Candle: domain.Candle{Timestamp: time.Unix(int64(i)*300, 0), Close: dec("100")},
		})
	}

	pt := newTestPaperTrader(repo, nil)
	pt.Bars = []string{"5m"}
	pt.CandleWindow = 10
	pt.candles = make(map[string][]domain.Candle)
	pt.seedCandlesFromRepo(ctx, testLogger())

	if got := len(pt.candles["5m"]); got != 10 {
		t.Fatalf("expected the window to be capped at CandleWindow=10, got %d", got)
	}
}

// Two strategies on the same token+bar disagreeing must NOT produce a simultaneous long and short
// (CLAUDE.md §15.12: a buy/sell decision exists only when no baseline position is open). Observed
// in production 2026-08-29 as orders 44 (sell, grid_like) and 45 (buy, weekly_dip_buy) coexisting
// on TRUMP-USDT-SWAP/5m — positions that cannot both be right and that no lifecycle decision
// authorized. The guard has to hold WITHIN one evaluation pass too, since the open-order list is
// read once before the strategy loop.
func TestEvaluateStrategies_DoesNotOpenOpposingPositionsInOnePass(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	alwaysSell := &stubStrategy{signal: strategy.Signal{Side: strategy.Sell, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{
		{Bar: "5m", Strategy: alwaysBuy},
		{Bar: "5m", Strategy: alwaysSell},
	})
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}

	if err := pt.evaluateStrategies(context.Background(), "5m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected exactly 1 open order, got %d (opposing positions on the same token)", len(open))
	}
}

// Panel control-box "paused"/"stopped" state (CLAUDE.md): TradingPaused must stop new opens
// outright, without even touching the repository's open-order list.
func TestEvaluateStrategies_TradingPausedOpensNothing(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "5m", Strategy: alwaysBuy}})
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}
	pt.TradingPaused = true

	if err := pt.evaluateStrategies(context.Background(), "5m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 0 {
		t.Fatalf("expected no orders opened while paused, got %d", len(open))
	}
}

// Panel control-box per-token disable (CLAUDE.md): OpensDisabled stops new opens on this
// instrument, but an existing open position must still be monitorable/closable normally — this
// test only asserts the open-gate side; monitorOpenOrders is untouched by either flag.
func TestEvaluateStrategies_OpensDisabledStopsNewOpensOnly(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "5m", Strategy: alwaysBuy}})
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}
	pt.OpensDisabled = true

	if err := pt.evaluateStrategies(context.Background(), "5m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 0 {
		t.Fatalf("expected no orders opened for a disabled token, got %d", len(open))
	}
}

// Panel control-box long/short toggle (CLAUDE.md): a disabled side's signal must not open a
// position, but the opposite side must still work normally in the same pass.
func TestEvaluateStrategies_DisableLongSkipsBuySignalsOnly(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "5m", Strategy: alwaysBuy}})
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}
	pt.DisableLong = true

	if err := pt.evaluateStrategies(context.Background(), "5m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 0 {
		t.Fatalf("expected no buy orders opened while long is disabled, got %d", len(open))
	}
}

func TestEvaluateStrategies_DisableShortSkipsSellSignalsOnly(t *testing.T) {
	repo := newFakeRepository()
	alwaysSell := &stubStrategy{signal: strategy.Signal{Side: strategy.Sell, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "5m", Strategy: alwaysSell}})
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}
	pt.DisableShort = true

	if err := pt.evaluateStrategies(context.Background(), "5m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 0 {
		t.Fatalf("expected no sell orders opened while short is disabled, got %d", len(open))
	}
}

// A strategy that emits only a target must not produce a position with no stop. Observed as order
// 80 (TRUMP-USDT-SWAP, sell, stoch_cross): the strategy sets TPPct and never SLPct, ResolveLevels
// has nothing to derive a stop from, buildPaperOrder writes nil, and the order opened with
// unbounded downside. The clamps existed but were only reachable inside openDecision, which
// returns immediately when rl_sizing is off — so with the model out of the open path, nothing
// validated anything.
func TestEvaluateStrategies_NeverOpensWithoutStopLoss(t *testing.T) {
	repo := newFakeRepository()
	targetOnly := &stubStrategy{signal: strategy.Signal{
		Side: strategy.Sell, TPPct: dec("0.01"), // no SLPct at all, exactly like stoch_cross
	}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "5m", Strategy: targetOnly}})
	pt.RLSizing = false // the configuration order 80 opened under
	pt.RLClamps = conductor.Clamps{
		MinSLDistPct: dec("0.005"), MaxSLDistPct: dec("0.05"), MinTPSLRatio: dec("1.5"),
	}
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}

	if err := pt.evaluateStrategies(context.Background(), "5m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected the signal to still be traded, got %d orders", len(open))
	}
	if open[0].SLPx == nil || !open[0].SLPx.IsPositive() {
		t.Fatal("order opened with no stop-loss: unbounded downside")
	}
	// A short's stop sits above entry.
	if !open[0].SLPx.GreaterThan(dec("100")) {
		t.Errorf("short's stop must be above entry, got %s", open[0].SLPx)
	}

	// The filled stop must still be subject to the TP:SL ratio. Apply skips that check when there
	// is no stop to measure against, so a stop filled AFTER Apply leaves the ratio unchecked —
	// which produced live orders with a 5% stop against a 1% target (0.2 reward:risk).
	if open[0].TPPx == nil {
		t.Fatal("expected a target")
	}
	slDist := open[0].SLPx.Sub(open[0].EntryPx).Abs()
	tpDist := open[0].EntryPx.Sub(*open[0].TPPx).Abs()
	if minTP := slDist.Mul(dec("1.5")); tpDist.LessThan(minTP) {
		t.Errorf("TP:SL ratio below MinTPSLRatio: sl=%s tp=%s (target must be >= %s from entry)",
			slDist, tpDist, minTP)
	}
}

// Each bar has its own consumer goroutine, so two timeframes whose candles close at the same
// instant must not both open a position on the same token. Observed in production as orders 70
// (15m) and 71 (5m) on ENA-USDT-SWAP, 13ms apart: the no-open-position guard covered a single
// evaluateStrategies call but nothing serialized the check against a concurrent one. Run with
// -race to catch a regression here.
func TestEvaluateStrategies_ConcurrentBarsDoNotBothOpen(t *testing.T) {
	repo := newFakeRepository()
	buy5m := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	buy15m := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{
		{Bar: "5m", Strategy: buy5m},
		{Bar: "15m", Strategy: buy15m},
	})
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}
	pt.candles["15m"] = []domain.Candle{{Close: dec("100")}}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, bar := range []string{"5m", "15m"} {
		wg.Add(1)
		go func(bar string) {
			defer wg.Done()
			<-start // release both goroutines together to maximize overlap
			_ = pt.evaluateStrategies(context.Background(), bar, dec("100"), testLogger())
		}(bar)
	}
	close(start)
	wg.Wait()

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("two bars closing together must open exactly 1 position, got %d", len(open))
	}
}

// A signal firing while a position is already open is an `update` about that position, not a new
// order — so a second evaluation pass must not stack another one on top.
func TestEvaluateStrategies_SkipsOpenWhenBaselineAlreadyOpen(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "5m", Strategy: alwaysBuy}})
	pt.candles["5m"] = []domain.Candle{{Close: dec("100")}}

	for i := 0; i < 3; i++ {
		if err := pt.evaluateStrategies(context.Background(), "5m", dec("100"), testLogger()); err != nil {
			t.Fatalf("evaluateStrategies returned error: %v", err)
		}
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected 1 open order after 3 passes, got %d", len(open))
	}
}

// A fork shadows its baseline parent rather than being a separate position (CLAUDE.md §15.4), so
// one left behind after its parent closed must not make the token look permanently occupied.
func TestHasOpenBaseline_IgnoresForks(t *testing.T) {
	if hasOpenBaseline([]port.PaperOrder{{Variant: "rl_adjusted"}}) {
		t.Error("a fork alone must not count as an open baseline position")
	}
	if !hasOpenBaseline([]port.PaperOrder{{Variant: "rl_adjusted"}, {Variant: "baseline"}}) {
		t.Error("a baseline alongside a fork must count")
	}
	if !hasOpenBaseline([]port.PaperOrder{{Variant: ""}}) {
		t.Error("an empty variant is a baseline (pre-fork rows) and must count")
	}
}

func TestMonitorOpenOrders_ClosesOnSLHit(t *testing.T) {
	repo := newFakeRepository()
	sl := dec("99")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	})

	pt := newTestPaperTrader(repo, nil)
	if err := pt.monitorOpenOrders(context.Background(), dec("99"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	closed := repo.orders[id]
	if closed.ClosedAt == nil {
		t.Fatal("expected order to be closed after SL hit")
	}
	if closed.CloseReason == nil || *closed.CloseReason != "sl" {
		t.Errorf("expected close reason 'sl', got %v", closed.CloseReason)
	}
}

// Regression coverage for the 2026-09-01 incident (CLAUDE.md): a 20x-leverage position opened with
// a naive 5% price-distance stop (order 636's exact numbers) must be tightened in-place, on the
// very next tick, to respect MaxLossPct — self-healing an already-open position without a manual
// DB edit, since the open-time clamp fix alone only protects orders opened AFTER it's deployed.
func TestMonitorOpenOrders_TightensAnOverWideStopToMaxLossPct(t *testing.T) {
	repo := newFakeRepository()
	entry := dec("0.004533")
	sl := dec("0.00430635") // order 636's actual stop: 5% below entry
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: entry, SLPx: &sl,
		Size: dec("10"), Leverage: dec("20"), OpenedAt: time.Now(),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.RLClamps = conductor.Clamps{MaxSLDistPct: dec("0.05"), MaxLossPct: dec("0.15")}

	// Above both the original stop (0.00430635) AND the corrected one (~0.0044990025, MaxLossPct
	// 0.15/20x = 0.75% below entry) — the order must stay open here either way; this test only
	// asserts the STOP ITSELF moved, not that the position closed.
	livePrice := dec("0.0045200")
	if err := pt.monitorOpenOrders(context.Background(), livePrice, testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	updated := repo.orders[id]
	if updated.ClosedAt != nil {
		t.Fatalf("expected the order to still be open (tightened, not closed) at price %s, got closed with reason %v", livePrice, updated.CloseReason)
	}
	if updated.SLPx == nil {
		t.Fatal("expected SLPx to remain set after tightening")
	}
	actualLossPct := entry.Sub(*updated.SLPx).Div(entry).Mul(dec("20"))
	maxAllowed := dec("0.15")
	if actualLossPct.GreaterThan(maxAllowed) {
		t.Errorf("stop %s still realizes %s loss at 20x, want <= %s (MaxLossPct not applied to an already-open position)",
			updated.SLPx, actualLossPct, maxAllowed)
	}
	if updated.SLPx.Equal(sl) {
		t.Error("expected the stop to have moved from its original 5-percent-distance value, it did not")
	}
}

// A stop that's already inside MaxLossPct/MaxSLDistPct must be left untouched — tightening must not
// fire on every tick for every order, only on ones that actually violate the cap.
func TestMonitorOpenOrders_DoesNotTightenAnAlreadySafeStop(t *testing.T) {
	repo := newFakeRepository()
	entry := dec("100")
	sl := dec("99.5") // 0.5% distance at 20x = 10% loss, already within MaxLossPct=0.15
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: entry, SLPx: &sl,
		Size: dec("100"), Leverage: dec("20"), OpenedAt: time.Now(),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.RLClamps = conductor.Clamps{MaxSLDistPct: dec("0.05"), MaxLossPct: dec("0.15")}

	if err := pt.monitorOpenOrders(context.Background(), dec("100"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	updated := repo.orders[id]
	if updated.SLPx == nil || !updated.SLPx.Equal(sl) {
		t.Errorf("expected the already-safe stop to remain unchanged at %s, got %v", sl, updated.SLPx)
	}
}

// The tightened stop must apply on the SAME tick, before the touch check — otherwise a position
// whose price has already crossed the corrected (tighter) level, but not the original wider one,
// would incorrectly stay open for one more tick.
func TestMonitorOpenOrders_TightenedStopAppliesOnTheSameTick(t *testing.T) {
	repo := newFakeRepository()
	entry := dec("100")
	sl := dec("95") // 5% distance at 20x = 100% loss (violates MaxLossPct)
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: entry, SLPx: &sl,
		Size: dec("100"), Leverage: dec("20"), OpenedAt: time.Now(),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.RLClamps = conductor.Clamps{MaxSLDistPct: dec("0.05"), MaxLossPct: dec("0.15")}
	// Corrected stop at MaxLossPct=0.15/20x = 0.75% distance -> 99.25. A price of 99.2 is below the
	// corrected stop (should trigger a close) but still above the original, wider 95 stop.
	livePrice := dec("99.2")

	if err := pt.monitorOpenOrders(context.Background(), livePrice, testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	updated := repo.orders[id]
	if updated.ClosedAt == nil {
		t.Fatal("expected the order to close on the same tick its stop was tightened past the live price")
	}
	if updated.CloseReason == nil || *updated.CloseReason != "sl" {
		t.Errorf("expected close reason 'sl', got %v", updated.CloseReason)
	}
}

// A manual close request from the panel (2026-08-31) wins over everything else: even a position
// that hasn't touched SL/TP and isn't timed out must close the moment the flag is set.
func TestMonitorOpenOrders_ClosesOnManualCloseRequest(t *testing.T) {
	repo := newFakeRepository()
	sl := dec("50") // far away — never touched by the test's price
	tp := dec("500")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"),
	})
	if err := repo.RequestManualClose(context.Background(), id); err != nil {
		t.Fatalf("RequestManualClose: %v", err)
	}

	pt := newTestPaperTrader(repo, nil)
	if err := pt.monitorOpenOrders(context.Background(), dec("103"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	closed := repo.orders[id]
	if closed.ClosedAt == nil {
		t.Fatal("expected the order to close once manual close was requested")
	}
	if closed.CloseReason == nil || *closed.CloseReason != conductor.CloseReasonManual {
		t.Errorf("expected close reason %q, got %v", conductor.CloseReasonManual, closed.CloseReason)
	}
	if closed.RealizedPnL == nil || !closed.RealizedPnL.Equal(dec("3")) {
		t.Errorf("expected realized pnl at the live price (100->103, 1x, want 3), got %v", closed.RealizedPnL)
	}
}

// A manual close request must win even on the SAME tick a genuine SL/TP touch would also fire —
// the operator explicitly asked to exit now, so the reason recorded is 'manual', not 'sl'/'tp'.
func TestMonitorOpenOrders_ManualCloseTakesPriorityOverSLTPTouch(t *testing.T) {
	repo := newFakeRepository()
	sl := dec("99")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	})
	if err := repo.RequestManualClose(context.Background(), id); err != nil {
		t.Fatalf("RequestManualClose: %v", err)
	}

	pt := newTestPaperTrader(repo, nil)
	// Price is AT the SL level, so a real touch would also fire this same tick.
	if err := pt.monitorOpenOrders(context.Background(), dec("99"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	closed := repo.orders[id]
	if closed.CloseReason == nil || *closed.CloseReason != conductor.CloseReasonManual {
		t.Errorf("expected manual close to win over a coincidental SL touch, got %v", closed.CloseReason)
	}
}

func TestMonitorOpenOrders_ForceClosesAfterMaxOpenDuration(t *testing.T) {
	repo := newFakeRepository()
	sl := dec("50") // far away, so this test only ever exercises the timeout path, never SL/TP
	tp := dec("500")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), OpenedAt: time.Now().Add(-7 * time.Hour),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.MaxOpenDuration = 6 * time.Hour
	if err := pt.monitorOpenOrders(context.Background(), dec("100"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	closed := repo.orders[id]
	if closed.ClosedAt == nil {
		t.Fatal("expected the 7h-old order to be force-closed")
	}
	if closed.CloseReason == nil || *closed.CloseReason != conductor.CloseReasonTimeout {
		t.Errorf("expected close reason %q, got %v", conductor.CloseReasonTimeout, closed.CloseReason)
	}
}

func TestMonitorOpenOrders_DoesNotCloseBeforeMaxOpenDuration(t *testing.T) {
	repo := newFakeRepository()
	sl := dec("50")
	tp := dec("500")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("100"), Leverage: dec("1"), OpenedAt: time.Now().Add(-5 * time.Hour),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.MaxOpenDuration = 6 * time.Hour
	if err := pt.monitorOpenOrders(context.Background(), dec("100"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	if repo.orders[id].ClosedAt != nil {
		t.Fatal("a 5h-old order must not be force-closed against a 6h limit")
	}
}

func TestMonitorOpenOrders_SLTPTouchTakesPriorityOverTimeout(t *testing.T) {
	// A position that is BOTH stale AND has its SL/TP genuinely touched on this exact tick must
	// close with the real reason, not get relabeled 'timeout' just because it also happens to be
	// old -- the touch is checked first and only falls through to the timeout check when neither
	// level was hit.
	repo := newFakeRepository()
	sl := dec("99")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl,
		Size: dec("100"), Leverage: dec("1"), OpenedAt: time.Now().Add(-7 * time.Hour),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.MaxOpenDuration = 6 * time.Hour
	if err := pt.monitorOpenOrders(context.Background(), dec("99"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	closed := repo.orders[id]
	if closed.CloseReason == nil || *closed.CloseReason != "sl" {
		t.Errorf("expected the genuine SL touch to win, got %v", closed.CloseReason)
	}
}

// stubStrategy always returns the configured signal, ignoring the candle input.
type stubStrategy struct{ signal strategy.Signal }

func (s *stubStrategy) Name() string                                            { return "stub" }
func (s *stubStrategy) Params() []strategy.ParamSpec                            { return nil }
func (s *stubStrategy) WithParams(map[string]decimal.Decimal) strategy.Strategy { return s }
func (s *stubStrategy) Evaluate(candles []strategy.Candle) (strategy.Signal, error) {
	return s.signal, nil
}

func TestHandleCandle_ParsesAndAppendsToWindow(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)

	event := candleEvent{
		InstID: "BTC-USDT-SWAP",
		Bar:    "1m",
		Candle: []string{"1700000000000", "100", "101", "99", "100.5", "10", "", "", "1"},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	if err := pt.handleCandle(context.Background(), "1m", data, testLogger()); err != nil {
		t.Fatalf("handleCandle returned error: %v", err)
	}

	if len(pt.candles["1m"]) != 1 {
		t.Fatalf("expected 1 candle in the 1m window, got %d", len(pt.candles["1m"]))
	}
	if !pt.candles["1m"][0].Close.Equal(dec("100.5")) {
		t.Errorf("expected close=100.5, got %s", pt.candles["1m"][0].Close)
	}
	if len(pt.candles["15m"]) != 0 {
		t.Errorf("expected the 15m window untouched by a 1m candle event, got %d entries", len(pt.candles["15m"]))
	}
	if len(repo.candles) != 1 {
		t.Errorf("expected finalized candle to be persisted, got %d saved", len(repo.candles))
	}
	if repo.candles[0].Bar != "1m" {
		t.Errorf("expected persisted candle to be tagged bar=1m, got %q", repo.candles[0].Bar)
	}
}

func TestHandleCandle_DifferentBarsMaintainIndependentWindows(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)

	oneMin := candleEvent{InstID: "BTC-USDT-SWAP", Bar: "1m", Candle: []string{"1700000000000", "100", "101", "99", "100.5", "10", "", "", "1"}}
	fifteenMin := candleEvent{InstID: "BTC-USDT-SWAP", Bar: "15m", Candle: []string{"1700000000000", "200", "201", "199", "200.5", "20", "", "", "1"}}

	oneMinData, _ := json.Marshal(oneMin)
	fifteenMinData, _ := json.Marshal(fifteenMin)

	if err := pt.handleCandle(context.Background(), "1m", oneMinData, testLogger()); err != nil {
		t.Fatalf("handleCandle(1m) returned error: %v", err)
	}
	if err := pt.handleCandle(context.Background(), "15m", fifteenMinData, testLogger()); err != nil {
		t.Fatalf("handleCandle(15m) returned error: %v", err)
	}

	if len(pt.candles["1m"]) != 1 || !pt.candles["1m"][0].Close.Equal(dec("100.5")) {
		t.Errorf("expected 1m window to have exactly the 1m candle, got %v", pt.candles["1m"])
	}
	if len(pt.candles["15m"]) != 1 || !pt.candles["15m"][0].Close.Equal(dec("200.5")) {
		t.Errorf("expected 15m window to have exactly the 15m candle, got %v", pt.candles["15m"])
	}
	if len(repo.candles) != 2 {
		t.Fatalf("expected both candles persisted, got %d", len(repo.candles))
	}
}

// TestHandleCandle_ConcurrentBarsDoNotRace is a regression test for a real crash found during
// the multi-timeframe dry run: each bar gets its own consumer goroutine (see Run), so concurrent
// handleCandle calls for different bars write to the shared e.candles map at the same time —
// caught in production as "fatal error: concurrent map writes" (a Go runtime crash, not just a
// -race warning). Run with `go test -race` to actually catch a regression here; without -race
// this only proves the code doesn't deadlock/panic under load, not that it's race-free.
func TestHandleCandle_ConcurrentBarsDoNotRace(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.Bars = []string{"1m", "15m", "1H"}
	pt.candles = map[string][]domain.Candle{"1m": nil, "15m": nil, "1H": nil}

	makeEvent := func(bar string, closePx string) []byte {
		e := candleEvent{InstID: "BTC-USDT-SWAP", Bar: bar, Candle: []string{"1700000000000", "100", "101", "99", closePx, "10", "", "", "1"}}
		data, _ := json.Marshal(e)
		return data
	}

	var wg sync.WaitGroup
	for _, bar := range []string{"1m", "15m", "1H"} {
		bar := bar
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				data := makeEvent(bar, "100.5")
				if err := pt.handleCandle(context.Background(), bar, data, testLogger()); err != nil {
					t.Errorf("handleCandle(%s) returned error: %v", bar, err)
				}
			}(i)
		}
	}
	wg.Wait()

	for _, bar := range []string{"1m", "15m", "1H"} {
		if len(pt.candles[bar]) == 0 {
			t.Errorf("expected candles recorded for bar %s after concurrent writes, got none", bar)
		}
	}
}

// fakeModelClientRL returns a fixed action on every Predict call — used to test the SL/TP
// adjustment/fork path deterministically.
type fakeModelClientRL struct {
	action domain.Action
	calls  int
}

func (f *fakeModelClientRL) Predict(ctx context.Context, obs domain.Observation) (*domain.Action, error) {
	f.calls++
	a := f.action
	return &a, nil
}

func TestRunUpdates_ForksOnNonZeroAdjustment(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {
		{Close: dec("100")}, {Close: dec("101")}, {Close: dec("102")}, {Close: dec("105")},
	}, "15m": nil}
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionUpdate, SLPx: dec("97"), TPPx: dec("108")}}
	pt.Model = model

	sl, tp := dec("95"), dec("110")
	id, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp, Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	pt.runUpdates(context.Background(), "1m", dec("105"), testLogger())

	if model.calls == 0 {
		t.Fatalf("expected Predict to be called")
	}

	orders, err := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("list open orders: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("expected baseline + 1 fork = 2 open orders, got %d", len(orders))
	}

	var baseline, fork *port.PaperOrder
	for i := range orders {
		o := orders[i]
		if o.ID == id {
			baseline = &o
		} else {
			fork = &o
		}
	}
	if baseline == nil || fork == nil {
		t.Fatalf("expected to find both baseline (id=%d) and a fork among %+v", id, orders)
	}
	if !baseline.SLPx.Equal(sl) || !baseline.TPPx.Equal(tp) {
		t.Errorf("expected baseline order's SL/TP untouched, got sl=%s tp=%s", baseline.SLPx, baseline.TPPx)
	}
	if fork.Variant != "rl_adjusted" || fork.ParentOrderID == nil || *fork.ParentOrderID != id {
		t.Errorf("expected fork tagged rl_adjusted with parent %d, got variant=%s parent=%v", id, fork.Variant, fork.ParentOrderID)
	}
	// SL should have tightened (moved up from 95, toward locking profit at price 105).
	if !fork.SLPx.GreaterThan(sl) {
		t.Errorf("expected fork's SL to have tightened above %s, got %s", sl, fork.SLPx)
	}
}

// A baseline gets at most ONE fork: §15.4's mechanic is a same-entry A/B, one control against one
// adjusted variant, and a second fork of the same parent destroys that comparison. Observed
// 2026-08-29 as 49 forks across only 12 baselines — HYPE-USDT-SWAP order 51 alone had 8, since
// each fork is itself an open position drawing its own update calls, so the branching compounded.
// Further adjustments must EDIT the existing fork instead.
func TestRunUpdates_SecondAdjustmentEditsForkInsteadOfBranching(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {
		{Close: dec("100")}, {Close: dec("101")}, {Close: dec("102")}, {Close: dec("105")},
	}, "15m": nil}
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionUpdate, SLPx: dec("97"), TPPx: dec("108")}}
	pt.Model = model

	sl, tp := dec("95"), dec("110")
	baselineID, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp, Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	// Three adjustment rounds, each proposing a tighter stop than the last.
	for i, proposed := range []string{"97", "99", "101"} {
		model.action = domain.Action{Action: domain.ActionUpdate, SLPx: dec(proposed), TPPx: dec("108")}
		pt.lifecycle = nil // clear the conductor's per-order update cadence so each round fires
		pt.conductorOnce = sync.Once{}
		pt.runUpdates(context.Background(), "1m", dec("105"), testLogger())

		orders, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
		forks := 0
		for _, o := range orders {
			if o.Variant == "rl_adjusted" {
				forks++
			}
		}
		if forks > 1 {
			t.Fatalf("round %d: expected at most 1 fork per baseline, got %d", i+1, forks)
		}
	}

	orders, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(orders) != 2 {
		t.Fatalf("expected baseline + exactly 1 fork = 2 orders, got %d", len(orders))
	}
	for _, o := range orders {
		if o.ID == baselineID && (!o.SLPx.Equal(sl) || !o.TPPx.Equal(tp)) {
			t.Errorf("baseline must stay untouched, got sl=%s tp=%s", o.SLPx, o.TPPx)
		}
	}
}

func TestRunUpdates_NoOpActionDoesNotFork(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	pt.Model = &fakeModelClientRL{action: domain.Action{}} // zero adjustment, matches the no-op fail-safe

	sl := dec("95")
	_, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	pt.runUpdates(context.Background(), "1m", dec("100"), testLogger())

	orders, err := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("list open orders: %v", err)
	}
	if len(orders) != 1 {
		t.Errorf("expected no fork for a zero-adjustment action, got %d open orders", len(orders))
	}
}

func TestRunUpdates_SkipsWhenNoOpenOrders(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionUpdate, SLPx: dec("97")}}
	pt.Model = model

	pt.runUpdates(context.Background(), "1m", dec("100"), testLogger())

	if model.calls != 0 {
		t.Errorf("expected Predict never called when there are no open baseline orders, got %d calls", model.calls)
	}
}

// TestHandleTick_TriggersRLAdjustOnLiveTickPrice covers CLAUDE.md §15.9's freshness fix: the RL
// SL/TP-adjust pass must fire from the tick stream (using the live tick price), not only at
// candle close — this is the actual behavioral change, previously uncovered by any test since
// TestRunUpdates_* above call runUpdates directly rather than through handleTick.
func TestHandleTick_TriggersRLAdjustOnLiveTickPrice(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	model := &fakeModelClientRL{action: domain.Action{}}
	pt.Model = model
	pt.RLSLTPAdjust = true

	sl := dec("95")
	_, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	tick, _ := json.Marshal(tickEvent{InstID: "BTC-USDT-SWAP", Last: "103.5"})
	if err := pt.handleTick(context.Background(), tick, testLogger()); err != nil {
		t.Fatalf("handleTick: %v", err)
	}

	if model.calls != 1 {
		t.Fatalf("expected Predict called once from a tick with an open baseline order, got %d calls", model.calls)
	}
}

// TestHandleTick_RLAdjustThrottled covers the RLAdjustInterval throttle: a burst of ticks within
// the same interval must only trigger one RL SL/TP-adjust pass, so a busy token doesn't call
// rl_service on every single tick (CLAUDE.md §15.9).
func TestHandleTick_RLAdjustThrottled(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	model := &fakeModelClientRL{action: domain.Action{}}
	pt.Model = model
	pt.RLSLTPAdjust = true

	sl := dec("95")
	_, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	for i := 0; i < 5; i++ {
		tick, _ := json.Marshal(tickEvent{InstID: "BTC-USDT-SWAP", Last: "103.5"})
		if err := pt.handleTick(context.Background(), tick, testLogger()); err != nil {
			t.Fatalf("handleTick #%d: %v", i, err)
		}
	}

	if model.calls != 1 {
		t.Errorf("expected exactly 1 Predict call across a burst of ticks within RLAdjustInterval, got %d", model.calls)
	}
}

func TestSLTPAdjustmentStats_AggregatesPairedTradesOnly(t *testing.T) {
	repo := newFakeRepository()
	ctx := context.Background()

	// Baseline #1: has a fork, both closed. Baseline wins (tp), fork loses (sl). The fork must be
	// created (ForkPaperOrderWithSLTP requires an open parent) BEFORE the baseline is closed.
	closedAt1 := time.Now()
	pnl1 := dec("5")
	tpReason := "tp"
	baseline1ID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})

	forkPnl1 := dec("-2")
	slReason := "sl"
	fork1ID, err := repo.ForkPaperOrderWithSLTP(ctx, baseline1ID, ptr(dec("95")), ptr(dec("110")))
	if err != nil {
		t.Fatalf("fork baseline1: %v", err)
	}
	repo.mu.Lock()
	f1 := repo.orders[fork1ID]
	f1.ClosedAt = &closedAt1
	f1.CloseReason = &slReason
	f1.RealizedPnL = &forkPnl1
	repo.orders[fork1ID] = f1
	repo.mu.Unlock()

	repo.mu.Lock()
	b1 := repo.orders[baseline1ID]
	b1.ClosedAt = &closedAt1
	b1.CloseReason = &tpReason
	b1.RealizedPnL = &pnl1
	repo.orders[baseline1ID] = b1
	repo.mu.Unlock()

	// Baseline #2: no fork at all — must be excluded entirely from the comparison.
	pnl2 := dec("100")
	baseline2ID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})
	repo.mu.Lock()
	b2 := repo.orders[baseline2ID]
	b2.ClosedAt = &closedAt1
	b2.CloseReason = &tpReason
	b2.RealizedPnL = &pnl2
	repo.orders[baseline2ID] = b2
	repo.mu.Unlock()

	stats, err := repo.SLTPAdjustmentStats(ctx, "", time.Time{})
	if err != nil {
		t.Fatalf("SLTPAdjustmentStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 variant rows, got %d", len(stats))
	}

	var baseline, adjusted port.VariantStats
	for _, s := range stats {
		if s.Variant == "baseline" {
			baseline = s
		} else {
			adjusted = s
		}
	}

	if baseline.ClosedCount != 1 || baseline.Wins != 1 || !baseline.RealizedPnL.Equal(dec("5")) {
		t.Errorf("expected baseline{closed=1,wins=1,pnl=5} (baseline2 excluded, no fork), got %+v", baseline)
	}
	if adjusted.ClosedCount != 1 || adjusted.Losses != 1 || !adjusted.RealizedPnL.Equal(dec("-2")) {
		t.Errorf("expected rl_adjusted{closed=1,losses=1,pnl=-2}, got %+v", adjusted)
	}
}

func TestListSLTPAdjustmentPairs_ReturnsLinkedPairsOnly(t *testing.T) {
	repo := newFakeRepository()
	ctx := context.Background()

	baselineID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})
	forkID, _ := repo.ForkPaperOrderWithSLTP(ctx, baselineID, ptr(dec("95")), ptr(dec("110")))
	// An unrelated unpaired baseline order should not show up as a pair.
	_, _ = repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})

	pairs, err := repo.ListSLTPAdjustmentPairs(ctx, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("ListSLTPAdjustmentPairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("expected exactly 1 pair, got %d", len(pairs))
	}
	if pairs[0].BaselineOrder.ID != baselineID || pairs[0].RLAdjustedOrder.ID != forkID {
		t.Errorf("expected pair baseline=%d fork=%d, got baseline=%d fork=%d",
			baselineID, forkID, pairs[0].BaselineOrder.ID, pairs[0].RLAdjustedOrder.ID)
	}
}

func TestEvaluateStrategies_PersistsDecisionTimeObservation(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02"), Confidence: dec("0.8")}}
	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "1m", Strategy: alwaysBuy, StrategyID: 7}})
	pt.candles["1m"] = []domain.Candle{
		{Close: dec("98")}, {Close: dec("99")}, {Close: dec("100")},
	}
	pt.ActiveTokens = []string{"BTC-USDT-SWAP", "XAU-USD-SWAP"}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected 1 opened order, got %d", len(open))
	}
	if len(open[0].FeaturesJSON) == 0 {
		t.Fatalf("expected FeaturesJSON to be populated with the decision-time observation")
	}

	var obs domain.Observation
	if err := json.Unmarshal(open[0].FeaturesJSON, &obs); err != nil {
		t.Fatalf("FeaturesJSON did not unmarshal as domain.Observation: %v", err)
	}
	if obs.SchemaVersion != domain.ObservationSchemaVersion {
		t.Errorf("expected schema version %d, got %d", domain.ObservationSchemaVersion, obs.SchemaVersion)
	}
	if obs.InstID != "BTC-USDT-SWAP" {
		t.Errorf("expected InstID BTC-USDT-SWAP, got %q", obs.InstID)
	}
	if len(obs.Timeframes) != 1 || len(obs.Timeframes[0].StrategySignals) != 1 {
		t.Fatalf("expected 1 timeframe block with 1 strategy signal, got %+v", obs.Timeframes)
	}
	if obs.Timeframes[0].StrategySignals[0].StrategyID != 7 {
		t.Errorf("expected strategy signal StrategyID=7, got %d", obs.Timeframes[0].StrategySignals[0].StrategyID)
	}
}

// errModelClient always fails, covering the RL-sizing fallback path: a model error must never
// block opening the order, it just falls back to the configured fixed sizing.
type errModelClient struct{ calls int }

func (f *errModelClient) Predict(ctx context.Context, obs domain.Observation) (*domain.Action, error) {
	f.calls++
	return nil, fmt.Errorf("rl-service unavailable")
}

func newSizingTestPaperTrader(repo port.Repository, model port.ModelClient) *PaperTrader {
	buy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "1m", Strategy: buy}})
	pt.candles["1m"] = []domain.Candle{{Open: dec("100"), High: dec("101"), Low: dec("99"), Close: dec("100"), Volume: dec("1")}}
	pt.Model = model
	pt.MaxLeverage = dec("100")
	return pt
}

func openedOrder(t *testing.T, repo port.Repository) port.PaperOrder {
	t.Helper()
	open, err := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if err != nil || len(open) != 1 {
		t.Fatalf("expected exactly 1 open order (err=%v), got %d", err, len(open))
	}
	return open[0]
}

// TestEvaluateStrategies_RLSizingSetsNotionalAndLeverage covers CLAUDE.md §15.4's sizing action:
// with rl_sizing on, a new order's size/leverage come from the model's TargetExposure/LeverageFrac
// instead of the fixed NotionalUSD at 1x — without this, the paper_orders log has no
// leverage/exposure variance for §15.8's continued-live-learning phase to learn sizing from.
func TestEvaluateStrategies_RLSizingSetsNotionalAndLeverage(t *testing.T) {
	repo := newFakeRepository()
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.1")}}
	pt := newSizingTestPaperTrader(repo, model)
	pt.RLSizing = true

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}

	o := openedOrder(t, repo)
	// 0.5 exposure of the $1000 account = $500, capped to MaxPositionPct (25% = $250).
	if !o.Size.Equal(dec("250")) {
		t.Errorf("expected size capped to 250 (25%% of a 1000 account), got %s", o.Size)
	}
	// leverage_frac 0.1 maps onto [1x, 100x]: 1 + 0.1*99
	if !o.Leverage.Equal(dec("10.9")) {
		t.Errorf("expected leverage 10.9 from frac 0.1 against a 100x cap, got %s", o.Leverage)
	}
}

// The strategy layer owns direction (CLAUDE.md §9/§16.1) — a negative TargetExposure against a buy
// signal must size the order, never flip it to a sell.
func TestEvaluateStrategies_RLSizingNeverFlipsSignalDirection(t *testing.T) {
	repo := newFakeRepository()
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.4"), LeverageFrac: dec("0")}}
	pt := newSizingTestPaperTrader(repo, model)
	pt.RLSizing = true

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}

	o := openedOrder(t, repo)
	if o.Side != "buy" {
		t.Errorf("expected the strategy's buy side preserved, got %q", o.Side)
	}
	// Magnitude only: |−0.4| * 1000 = 400, capped to 25% of equity = 250.
	if !o.Size.Equal(dec("250")) {
		t.Errorf("expected the exposure magnitude sized and capped to 250, got %s", o.Size)
	}
}

func TestEvaluateStrategies_RLSizingDisabledKeepsFixedSizing(t *testing.T) {
	repo := newFakeRepository()
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("1")}}
	pt := newSizingTestPaperTrader(repo, model) // RLSizing left false

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}

	o := openedOrder(t, repo)
	if !o.Size.Equal(dec("100")) || !o.Leverage.Equal(defaultPaperLeverage) {
		t.Errorf("expected the fixed 100 @ %sx while rl_sizing is off, got %s @ %sx", defaultPaperLeverage, o.Size, o.Leverage)
	}
	if model.calls != 0 {
		t.Errorf("expected the model never consulted while rl_sizing is off, got %d calls", model.calls)
	}
}

// A failing rl-service must degrade to the fixed sizing, never block the trade.
func TestEvaluateStrategies_RLSizingFallsBackWhenModelErrors(t *testing.T) {
	repo := newFakeRepository()
	model := &errModelClient{}
	pt := newSizingTestPaperTrader(repo, model)
	pt.RLSizing = true

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies must not fail when the model errors: %v", err)
	}

	o := openedOrder(t, repo)
	if !o.Size.Equal(dec("100")) || !o.Leverage.Equal(defaultPaperLeverage) {
		t.Errorf("expected fallback to the fixed 100 @ %sx, got %s @ %sx", defaultPaperLeverage, o.Size, o.Leverage)
	}
	if model.calls != 1 {
		t.Errorf("expected one Predict attempt before falling back, got %d", model.calls)
	}
}

// TestRLSizing_TotalExposureCeilingBlocksNewPosition covers the second Go-side cap (CLAUDE.md
// §15.6): the per-position cap alone still permits enough simultaneous positions to commit the
// whole account, so total open exposure is bounded independently.
func TestRLSizing_TotalExposureCeilingBlocksNewPosition(t *testing.T) {
	repo := newFakeRepository()
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("1"), LeverageFrac: dec("0")}}
	pt := newSizingTestPaperTrader(repo, model)
	pt.RLSizing = true

	// 600 already open == the whole 60% ceiling on a 1000 account: no headroom left.
	_, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Size: dec("600"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("seed open order: %v", err)
	}

	obs := domain.Observation{AccountEquityUSD: dec("1000")}
	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	_, _, ok := pt.rlSizing(context.Background(), obs, strategy.Signal{Side: strategy.Buy}, open, testLogger())

	if ok {
		t.Error("expected RL sizing to decline once the total-exposure ceiling is reached")
	}
}

func TestRLSizing_TrimsToRemainingExposureHeadroom(t *testing.T) {
	repo := newFakeRepository()
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("1"), LeverageFrac: dec("0")}}
	pt := newSizingTestPaperTrader(repo, model)
	pt.RLSizing = true

	// 500 open against a 600 ceiling leaves 100 of headroom, below the 250 per-position cap.
	_, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Size: dec("500"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("seed open order: %v", err)
	}

	obs := domain.Observation{AccountEquityUSD: dec("1000")}
	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	notional, _, ok := pt.rlSizing(context.Background(), obs, strategy.Signal{Side: strategy.Buy}, open, testLogger())

	if !ok {
		t.Fatal("expected sizing to succeed with headroom remaining")
	}
	if !notional.Equal(dec("100")) {
		t.Errorf("expected the position trimmed to the 100 of remaining headroom, got %s", notional)
	}
}

// Forks shadow their baseline parent rather than committing separate capital (CLAUDE.md §15.4), so
// counting them toward the exposure ceiling would double-charge one signal.
func TestRLSizing_ForksDoNotCountTowardExposureCeiling(t *testing.T) {
	repo := newFakeRepository()
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.1"), LeverageFrac: dec("0")}}
	pt := newSizingTestPaperTrader(repo, model)
	pt.RLSizing = true

	obs := domain.Observation{AccountEquityUSD: dec("1000")}
	open := []port.PaperOrder{
		{InstID: "BTC-USDT-SWAP", Size: dec("300"), Variant: "baseline"},
		{InstID: "BTC-USDT-SWAP", Size: dec("300"), Variant: "rl_adjusted"}, // must not count
	}
	notional, _, ok := pt.rlSizing(context.Background(), obs, strategy.Signal{Side: strategy.Buy}, open, testLogger())

	if !ok {
		t.Fatal("expected sizing to succeed: only the 300 baseline counts against the 600 ceiling")
	}
	if !notional.Equal(dec("100")) { // 0.1 * 1000, under both caps
		t.Errorf("expected 100, got %s", notional)
	}
}

func TestRLSizing_DeclinesOnDrainedAccount(t *testing.T) {
	repo := newFakeRepository()
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("1"), LeverageFrac: dec("0")}}
	pt := newSizingTestPaperTrader(repo, model)
	pt.RLSizing = true

	obs := domain.Observation{AccountEquityUSD: decimal.Zero}
	_, _, ok := pt.rlSizing(context.Background(), obs, strategy.Signal{Side: strategy.Buy}, nil, testLogger())

	if ok {
		t.Error("expected sizing to decline against a drained account rather than sizing off a stale constant")
	}
	if model.calls != 0 {
		t.Errorf("expected no model call when there's no equity to size against, got %d", model.calls)
	}
}

// TestMonitorOpenOrders_DrainedAccountResetsAndRecordsTimeline covers CLAUDE.md §15.7: a paper
// account drained to zero is topped back up, and — the part that matters for reviewing it after
// the fact — both the drop and the reset land in the equity timeline the panel charts.
func TestMonitorOpenOrders_DrainedAccountResetsAndRecordsTimeline(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.AccountInitialUSD = dec("100")

	if _, err := repo.GetAccountEquity(ctx, "paper", dec("100")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// A losing long: entry 100, SL 95, size 2000 => -100 realized, draining the account exactly.
	sl := dec("95")
	if _, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("2000"), Leverage: dec("1"),
	}); err != nil {
		t.Fatalf("open order: %v", err)
	}

	if err := pt.monitorOpenOrders(ctx, dec("95"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders: %v", err)
	}

	acct, _ := repo.GetAccountEquity(ctx, "paper", dec("100"))
	if !acct.EquityUSD.Equal(dec("100")) {
		t.Errorf("expected the drained account reset to its 100 initial, got %s", acct.EquityUSD)
	}
	if acct.ResetCount != 1 {
		t.Errorf("expected reset_count 1, got %d", acct.ResetCount)
	}

	history, err := repo.ListEquityHistory(ctx, "paper", time.Time{}, 0)
	if err != nil {
		t.Fatalf("list equity history: %v", err)
	}
	var sawTrade, sawReset bool
	for _, p := range history {
		switch p.Reason {
		case "trade":
			sawTrade = true
		case "reset":
			sawReset = true
		}
	}
	if !sawTrade || !sawReset {
		t.Errorf("expected both the losing trade and the reset in the timeline, got %+v", history)
	}
}

// Real money is never auto-topped-up (CLAUDE.md §15.7): running out is a stop condition for a
// human, not a bookkeeping event.
func TestApplyRealizedPnL_RealModeNeverAutoResets(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "real", dec("100")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	acct, reset, err := repo.ApplyRealizedPnL(ctx, "real", dec("-150"), nil, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("apply pnl: %v", err)
	}
	if reset {
		t.Error("real mode must never auto-reset a drained balance")
	}
	if !acct.EquityUSD.Equal(dec("-50")) {
		t.Errorf("expected the real balance left at -50, got %s", acct.EquityUSD)
	}
}

// TestDecisionBar_DefaultsToShortestConfiguredBar covers the replacement for the old Bars[0]
// selection (CLAUDE.md §15.9): a tick belongs to no single bar, so the decision context is chosen
// deliberately — the shortest timeframe, being the freshest read of what price is doing right now.
// Array order silently changing meaning when the config list is reordered was the old behavior.
func TestDecisionBar_DefaultsToShortestConfiguredBar(t *testing.T) {
	for _, tc := range []struct {
		name string
		bars []string
		want string
	}{
		{"ordered", []string{"5m", "15m", "1H"}, "5m"},
		{"reversed", []string{"1H", "15m", "5m"}, "5m"},
		{"hours only", []string{"4H", "1H"}, "1H"},
		{"single", []string{"15m"}, "15m"},
		{"with days", []string{"1D", "4H", "15m"}, "15m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt := &PaperTrader{Bars: tc.bars}
			if got := pt.decisionBar(); got != tc.want {
				t.Errorf("bars %v: want %q, got %q", tc.bars, tc.want, got)
			}
		})
	}
}

func TestDecisionBar_ExplicitConfigWins(t *testing.T) {
	pt := &PaperTrader{Bars: []string{"5m", "15m", "1H"}, RLDecisionBar: "1H"}
	if got := pt.decisionBar(); got != "1H" {
		t.Errorf("want the explicitly configured 1H, got %q", got)
	}
}

func TestBarSeconds_OrdersTimeframes(t *testing.T) {
	if barSeconds("5m") >= barSeconds("15m") {
		t.Error("5m must sort before 15m")
	}
	if barSeconds("15m") >= barSeconds("1H") {
		t.Error("15m must sort before 1H")
	}
	if barSeconds("1H") >= barSeconds("4H") {
		t.Error("1H must sort before 4H")
	}
	if barSeconds("4H") >= barSeconds("1D") {
		t.Error("4H must sort before 1D")
	}
	// Case-insensitive on the unit: a mis-cased bar is rejected by config validation, but ordering
	// must not silently misbehave if one reaches here.
	if barSeconds("1h") != barSeconds("1H") {
		t.Error("hour ordering must not depend on casing")
	}
	// 'm' is minutes, 'M' is months in OKX's scheme — these must not collide.
	if barSeconds("1m") >= barSeconds("1M") {
		t.Error("1m (minute) must sort well before 1M (month)")
	}
	if barSeconds("") != barSeconds("nonsense") {
		t.Error("unrecognized bars should both sort last")
	}
}

// A multi-timeframe strategy must receive every maintained bar, not just its own, when the engine
// evaluates it (CLAUDE.md §9).
func TestMarketView_CarriesAllMaintainedBars(t *testing.T) {
	pt := newTestPaperTrader(newFakeRepository(), nil)
	pt.candles = map[string][]domain.Candle{
		"5m":  {{Close: dec("1")}, {Close: dec("2")}},
		"15m": {{Close: dec("3")}},
		"1H":  {{Close: dec("4")}},
	}

	v := pt.marketView("5m")

	if v.Bar != "5m" {
		t.Errorf("want decision bar 5m, got %q", v.Bar)
	}
	if len(v.Candles) != 2 {
		t.Errorf("want the 5m window of 2, got %d", len(v.Candles))
	}
	if len(v.Bars) != 3 {
		t.Errorf("want all 3 maintained bars available, got %d", len(v.Bars))
	}
	if h, ok := v.Higher("1H", 1); !ok || len(h) != 1 {
		t.Errorf("want 1H context reachable, got ok=%v len=%d", ok, len(h))
	}
}

// marketView must hand out a snapshot: a strategy holding the returned slices must not observe
// later engine writes, and must not be able to mutate engine state.
func TestMarketView_IsASnapshot(t *testing.T) {
	pt := newTestPaperTrader(newFakeRepository(), nil)
	pt.candles = map[string][]domain.Candle{"5m": {{Close: dec("1")}}}

	v := pt.marketView("5m")
	pt.candlesMu.Lock()
	pt.candles["5m"] = append(pt.candles["5m"], domain.Candle{Close: dec("2")})
	pt.candlesMu.Unlock()

	if len(v.Bars["5m"]) != 1 {
		t.Errorf("snapshot must not see later appends, got %d candles", len(v.Bars["5m"]))
	}
}

// TestRunUpdates_RequiresUpdateOrderAction covers the §15.10 order-action head: the
// model has to actually ask to adjust. Without this gate a model meaning "leave it alone" would
// still fork whenever its adjust outputs happened to be nonzero, which for a continuous output is
// essentially always.
func TestRunUpdates_RequiresUpdateOrderAction(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	// Nonzero adjustments, but the model is saying "none" — no fork may be created.
	pt.Model = &fakeModelClientRL{action: domain.Action{
		Action: domain.ActionNone, SLPx: dec("97"), TPPx: dec("108"),
	}}

	sl := dec("95")
	if _, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	}); err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	pt.runUpdates(ctx, "1m", dec("103"), testLogger())

	open, _ := repo.ListOpenPaperOrders(ctx, "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Errorf("expected no fork when order_action is 'none', got %d open orders", len(open))
	}
}

// The observation must carry which strategy produced a signal and on which timeframe (§15.10) —
// one shared policy has no other way to tell strategies apart.
func TestBuildObservation_SignalsCarryKindAndBar(t *testing.T) {
	repo := newFakeRepository()
	buy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	pt := newTestPaperTrader(repo, []StrategyAssignment{
		{Bar: "1m", Strategy: buy, StrategyID: 7, Kind: "rsi_sma"},
	})
	pt.candles["1m"] = []domain.Candle{{Close: dec("100")}, {Close: dec("101")}}

	obs := pt.buildObservation(context.Background(), "1m", dec("101"), testLogger())

	if len(obs.Timeframes) != 1 || len(obs.Timeframes[0].StrategySignals) != 1 {
		t.Fatalf("expected one signal, got %+v", obs.Timeframes)
	}
	sig := obs.Timeframes[0].StrategySignals[0]
	if sig.Kind != "rsi_sma" {
		t.Errorf("want kind rsi_sma, got %q", sig.Kind)
	}
	if sig.Bar != "1m" {
		t.Errorf("want bar 1m, got %q", sig.Bar)
	}
	if obs.SchemaVersion != domain.ObservationSchemaVersion {
		t.Errorf("want schema v%d, got v%d", domain.ObservationSchemaVersion, obs.SchemaVersion)
	}
}

// A fork must be identifiable in the observation: fork outcomes are compared against their baseline
// parent (§15.4), so the model needs to know which it is reasoning about.
func TestPositionStateOf_MarksForks(t *testing.T) {
	sl, tp := dec("95"), dec("110")
	base := port.PaperOrder{EntryPx: dec("100"), SLPx: &sl, TPPx: &tp, Size: dec("25"), Leverage: dec("10"), Variant: "baseline"}
	fork := base
	fork.Variant = "rl_adjusted"

	price := dec("104")
	if positionStateOf(base, price).IsFork {
		t.Error("baseline must not be marked as a fork")
	}
	if !positionStateOf(fork, price).IsFork {
		t.Error("rl_adjusted variant must be marked as a fork")
	}

	// Entry/SL/TP live on the signal now (CLAUDE.md §15.11); what the position block adds is the
	// trade's own state — side, size, and where price sits relative to its levels.
	ps := positionStateOf(base, price)
	if !ps.PositionOpen || !ps.Side.Equal(dec("1")) || !ps.SizeUSD.Equal(dec("25")) {
		t.Errorf("position state did not carry the order's own state: %+v", ps)
	}
	if !ps.UnrealizedPnLPct.Equal(dec("0.4")) { // (104-100)/100 * 10x leverage on a long
		t.Errorf("want unrealized PnL 0.4, got %s", ps.UnrealizedPnLPct)
	}
}

// TestHandleCandle_FormingCandleReplacesRatherThanAppends covers the live-OHLC fix (CLAUDE.md
// §15.11). OKX pushes the same bar repeatedly as it forms; appending each push would fill the
// window with partial copies of one candle, and the observation's "live OHLC" would then be
// whichever partial copy happened to land last.
func TestHandleCandle_FormingCandleReplacesRatherThanAppends(t *testing.T) {
	ctx := context.Background()
	pt := newTestPaperTrader(newFakeRepository(), nil)

	ts := "1700000000000"
	forming := func(closePx string) []byte {
		b, _ := json.Marshal(candleEvent{
			InstID: "BTC-USDT-SWAP", Bar: "1m",
			Candle: []string{ts, "100", "105", "99", closePx, "10", "0", "0", "0"},
		})
		return b
	}

	for _, px := range []string{"101", "102", "103"} {
		if err := pt.handleCandle(ctx, "1m", forming(px), testLogger()); err != nil {
			t.Fatalf("handleCandle: %v", err)
		}
	}

	pt.candlesMu.Lock()
	window := pt.candles["1m"]
	pt.candlesMu.Unlock()

	if len(window) != 1 {
		t.Fatalf("expected one candle for one forming bar, got %d", len(window))
	}
	if !window[0].Close.Equal(dec("103")) {
		t.Errorf("window must hold the LATEST forming state, got close %s", window[0].Close)
	}
}

// A genuinely new bar must append rather than overwrite the previous one.
func TestHandleCandle_NewBarAppends(t *testing.T) {
	ctx := context.Background()
	pt := newTestPaperTrader(newFakeRepository(), nil)

	for _, ts := range []string{"1700000000000", "1700000060000"} {
		b, _ := json.Marshal(candleEvent{
			InstID: "BTC-USDT-SWAP", Bar: "1m",
			Candle: []string{ts, "100", "105", "99", "102", "10", "0", "0", "0"},
		})
		if err := pt.handleCandle(ctx, "1m", b, testLogger()); err != nil {
			t.Fatalf("handleCandle: %v", err)
		}
	}

	pt.candlesMu.Lock()
	n := len(pt.candles["1m"])
	pt.candlesMu.Unlock()

	if n != 2 {
		t.Errorf("expected two distinct bars to append, got %d", n)
	}
}

// The observation's price context must report the live forming candle's OHLC (CLAUDE.md §15.11) —
// on a 1H bar the last CLOSED candle can be an hour stale.
func TestBuildPriceContext_ReportsLiveCandleOHLC(t *testing.T) {
	pc := buildPriceContext([]domain.Candle{
		{Open: dec("90"), High: dec("95"), Low: dec("89"), Close: dec("94")},
		{Open: dec("94"), High: dec("99"), Low: dec("93"), Close: dec("98")}, // the forming bar
	})

	if !pc.Open.Equal(dec("94")) || !pc.High.Equal(dec("99")) ||
		!pc.Low.Equal(dec("93")) || !pc.Close.Equal(dec("98")) {
		t.Errorf("price context must carry the LAST (forming) candle's OHLC, got %+v", pc)
	}
}

// TestTrackPnLExtremes covers the pnl_max/pnl_min inputs (CLAUDE.md §15.11): a trade that ran deep
// into profit and round-tripped must still show that peak, which current PnL alone cannot express.
func TestTrackPnLExtremes_RecordsPeakAndTrough(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)

	id, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open order: %v", err)
	}

	// Runs to +8%, falls back to -3%, recovers to +1%.
	for _, px := range []string{"108", "97", "101"} {
		open, _ := repo.ListOpenPaperOrders(ctx, "BTC-USDT-SWAP")
		pt.trackPnLExtremes(ctx, open[0], dec(px), testLogger())
	}

	open, _ := repo.ListOpenPaperOrders(ctx, "BTC-USDT-SWAP")
	o := open[0]
	if o.ID != id {
		t.Fatalf("unexpected order %d", o.ID)
	}
	if !o.PnLMaxPct.Equal(dec("0.08")) {
		t.Errorf("want peak 0.08 retained after the round trip, got %s", o.PnLMaxPct)
	}
	if !o.PnLMinPct.Equal(dec("-0.03")) {
		t.Errorf("want trough -0.03, got %s", o.PnLMinPct)
	}
}

// Strategies may express SL/TP as levels or as percentages; the observation must always carry
// levels (CLAUDE.md §15.11).
func TestSignalResolveLevels_DerivesPricesFromPercentages(t *testing.T) {
	long := strategy.Signal{Side: strategy.Buy, SLPct: dec("0.02"), TPPct: dec("0.04")}.ResolveLevels(dec("100"))
	if !long.EntryPx.Equal(dec("100")) || !long.SLPx.Equal(dec("98")) || !long.TPPx.Equal(dec("104")) {
		t.Errorf("long: want entry 100 / SL 98 / TP 104, got %s / %s / %s", long.EntryPx, long.SLPx, long.TPPx)
	}

	// A short's stop sits ABOVE entry and its target below — the sign comes from the signal's side.
	short := strategy.Signal{Side: strategy.Sell, SLPct: dec("0.02"), TPPct: dec("0.04")}.ResolveLevels(dec("100"))
	if !short.SLPx.Equal(dec("102")) || !short.TPPx.Equal(dec("96")) {
		t.Errorf("short: want SL 102 / TP 96, got %s / %s", short.SLPx, short.TPPx)
	}
}

func TestSignalResolveLevels_KeepsExplicitLevels(t *testing.T) {
	// A strategy that read a real level off the chart must keep it — deriving over the top would
	// discard exactly the structure that made it a level.
	s := strategy.Signal{
		Side: strategy.Buy, SLPct: dec("0.02"), TPPct: dec("0.04"),
		SLPx: dec("93.5"), TPPx: dec("117"),
	}.ResolveLevels(dec("100"))

	if !s.SLPx.Equal(dec("93.5")) || !s.TPPx.Equal(dec("117")) {
		t.Errorf("explicit levels must survive resolution, got SL %s / TP %s", s.SLPx, s.TPPx)
	}
}
