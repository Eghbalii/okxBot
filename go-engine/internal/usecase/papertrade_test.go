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
	mu           sync.Mutex
	nextID       int64
	orders       map[int64]port.PaperOrder
	candles      []port.Candle
	accounts     map[string]port.AccountEquity
	equityPoints []port.EquityPoint
	// recordExchangeBalanceCalls counts calls, not their effect — see RecordExchangeBalance below.
	recordExchangeBalanceCalls int
	paramChanges               []port.ParamChange
	orderAdjustments           []port.PaperOrderAdjustment
	paperTradingConfig         map[string]*port.PaperTradingConfig
	fundingRates               []port.FundingRate

	// realOrders uses its own counter (nextRealID), deliberately NOT sharing nextID with the
	// paper orders map — real_orders and paper_orders are independent Postgres sequences post-
	// split (CLAUDE.md, real-trading readiness plan, 2026-09-04), so a test can construct the
	// exact cross-table id-collision scenario (paper order id=3 and real order id=3 coexisting)
	// production code must disambiguate correctly by mode/table, not by assuming ids are unique.
	nextRealID           int64
	realOrders           map[int64]port.RealOrder
	realOrderAdjustments []port.PaperOrderAdjustment
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		orders:     make(map[int64]port.PaperOrder),
		accounts:   make(map[string]port.AccountEquity),
		realOrders: make(map[int64]port.RealOrder),
	}
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
func (r *fakeRepository) ListAssignments(ctx context.Context, instID string, enabledOnly bool, mode string) ([]port.StrategyAssignment, error) {
	return nil, nil
}
func (r *fakeRepository) SetAssignmentEnabled(ctx context.Context, id int64, enabled bool) error {
	return nil
}
func (r *fakeRepository) DeleteAssignment(ctx context.Context, id int64) error { return nil }
func (r *fakeRepository) StrategyStatsFor(ctx context.Context, strategyID int64, mode string) (port.StrategyStats, error) {
	return port.StrategyStats{}, nil
}
func (r *fakeRepository) TokenStats24h(ctx context.Context, mode string) ([]port.TokenStats, error) {
	return nil, nil
}
func (r *fakeRepository) ListPositions(ctx context.Context, f port.PositionFilter) ([]port.PaperOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.PaperOrder
	for _, o := range r.orders {
		if f.Mode != "" && o.Mode != f.Mode {
			continue
		}
		if f.InstID != "" && o.InstID != f.InstID {
			continue
		}
		if f.Open != nil && (o.ClosedAt == nil) != *f.Open {
			continue
		}
		out = append(out, o)
	}
	if f.Limit > 0 && f.Offset < len(out) {
		end := f.Offset + f.Limit
		if end > len(out) {
			end = len(out)
		}
		out = out[f.Offset:end]
	}
	return out, nil
}
func (r *fakeRepository) CountPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	return 0, nil
}
func (r *fakeRepository) OpenPaperOrder(ctx context.Context, o port.PaperOrder) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	o.ID = r.nextID
	r.orders[o.ID] = o
	return o.ID, nil
}
func (r *fakeRepository) GetPaperOrder(ctx context.Context, id int64) (port.PaperOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok {
		return port.PaperOrder{}, fmt.Errorf("order %d not found", id)
	}
	return o, nil
}
func (r *fakeRepository) SetExchangeAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok {
		return fmt.Errorf("order %d not found", id)
	}
	aid := algoOrderID
	o.ExchangeAlgoOrderID = &aid
	r.orders[id] = o
	return nil
}
func (r *fakeRepository) ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL, feesUSD, fundingUSD decimal.Decimal) error {
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
	fees := feesUSD
	o.FeesUSD = &fees
	funding := fundingUSD
	o.FundingUSD = &funding
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
func (r *fakeRepository) GetPaperTradingConfig(ctx context.Context, mode string) (port.PaperTradingConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paperTradingConfig == nil || r.paperTradingConfig[mode] == nil {
		return port.PaperTradingConfig{TradingState: "running"}, nil
	}
	return *r.paperTradingConfig[mode], nil
}
func (r *fakeRepository) SavePaperTradingConfig(ctx context.Context, mode string, patch port.PaperTradingConfigPatch) (port.PaperTradingConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paperTradingConfig == nil {
		r.paperTradingConfig = make(map[string]*port.PaperTradingConfig)
	}
	c := port.PaperTradingConfig{TradingState: "running"}
	if r.paperTradingConfig[mode] != nil {
		c = *r.paperTradingConfig[mode]
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
	r.paperTradingConfig[mode] = &c
	return c, nil
}
func (r *fakeRepository) SetAssignmentsEnabledForKinds(ctx context.Context, mode string, activeKinds []string, instIDs, bars []string) error {
	return nil
}
func (r *fakeRepository) SaveFundingRates(ctx context.Context, rates []port.FundingRate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fundingRates = append(r.fundingRates, rates...)
	return nil
}
func (r *fakeRepository) SumFundingCost(ctx context.Context, instID string, openedAt, closedAt time.Time, notionalUSD decimal.Decimal) (decimal.Decimal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := decimal.Zero
	for _, fr := range r.fundingRates {
		if fr.InstID != instID {
			continue
		}
		if fr.FundingTime.Before(openedAt) || fr.FundingTime.After(closedAt) {
			continue
		}
		total = total.Add(fr.FundingRate)
	}
	return total.Mul(notionalUSD), nil
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
func (r *fakeRepository) RecordPaperOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal, source string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.orderAdjustments = append(r.orderAdjustments, port.PaperOrderAdjustment{
		ID: r.nextID, OrderID: orderID, Field: field, OldValue: oldValue, NewValue: newValue, Source: source,
	})
	return nil
}
func (r *fakeRepository) ListPaperOrderAdjustments(ctx context.Context, orderID int64) ([]port.PaperOrderAdjustment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.PaperOrderAdjustment
	for _, a := range r.orderAdjustments {
		if a.OrderID == orderID {
			out = append(out, a)
		}
	}
	return out, nil
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

func (r *fakeRepository) OpenRealOrder(ctx context.Context, o port.RealOrder) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextRealID++
	o.ID = r.nextRealID
	if o.Status == "" {
		o.Status = "pending"
	}
	r.realOrders[o.ID] = o
	return o.ID, nil
}
func (r *fakeRepository) GetRealOrder(ctx context.Context, id int64) (port.RealOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return port.RealOrder{}, fmt.Errorf("real order %d not found", id)
	}
	return o, nil
}
func (r *fakeRepository) UpdateRealOrderStatus(ctx context.Context, id int64, status string, entryPx, size, contracts *decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return fmt.Errorf("real order %d not found", id)
	}
	o.Status = status
	if entryPx != nil {
		o.EntryPx = *entryPx
	}
	if size != nil {
		o.Size = *size
	}
	// COALESCE semantics, matching the real SQL: a status-only transition must not blank a count
	// already recorded.
	if contracts != nil {
		o.Contracts = contracts
	}
	r.realOrders[id] = o
	return nil
}
func (r *fakeRepository) SetRealOrderFeatures(ctx context.Context, id int64, featuresJSON json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return fmt.Errorf("real order %d not found", id)
	}
	o.FeaturesJSON = featuresJSON
	r.realOrders[id] = o
	return nil
}
func (r *fakeRepository) SetRealOrderExchangeAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return fmt.Errorf("real order %d not found", id)
	}
	aid := algoOrderID
	o.ExchangeAlgoOrderID = &aid
	r.realOrders[id] = o
	return nil
}
func (r *fakeRepository) CloseRealOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.realOrders[id]
	// Same idempotency guard as the real SQL — see CloseRealOrderConfirmed below.
	if o.ClosedAt != nil {
		return fmt.Errorf("real order %d: %w", id, port.ErrOrderAlreadyClosed)
	}
	now := o.OpenedAt
	o.ClosedAt = &now
	o.CloseReason = &reason
	cp := closePx
	o.ClosePx = &cp
	pnl := realizedPnL
	o.RealizedPnL = &pnl
	r.realOrders[id] = o
	return nil
}
func (r *fakeRepository) UpdateRealOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal, manualOverride bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok || o.ClosedAt != nil {
		return nil
	}
	o.SLPx = slPx
	o.TPPx = tpPx
	if manualOverride {
		o.ManualOverride = true
	}
	r.realOrders[id] = o
	return nil
}
func (r *fakeRepository) ListOpenRealOrders(ctx context.Context, instID string) ([]port.RealOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.RealOrder
	for _, o := range r.realOrders {
		if o.InstID == instID && o.ClosedAt == nil && (o.Status == "filled" || o.Status == "partial") {
			out = append(out, o)
		}
	}
	return out, nil
}
func (r *fakeRepository) RequestRealManualClose(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok || o.ClosedAt != nil {
		return fmt.Errorf("real order %d is not open", id)
	}
	o.ManualCloseRequested = true
	r.realOrders[id] = o
	return nil
}
func (r *fakeRepository) RequestRealManualCloseAll(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, o := range r.realOrders {
		if o.ClosedAt == nil && (o.Status == "filled" || o.Status == "partial") {
			o.ManualCloseRequested = true
			r.realOrders[id] = o
			n++
		}
	}
	return n, nil
}
func (r *fakeRepository) SetRealOrderClosing(ctx context.Context, id int64, closeOrderID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return nil
	}
	o.Status = "closing"
	if closeOrderID != "" {
		o.ExchangeCloseOrderID = &closeOrderID
	}
	o.LastError, o.LastErrorAt = nil, nil
	r.realOrders[id] = o
	return nil
}

func (r *fakeRepository) CloseRealOrderConfirmed(ctx context.Context, id int64, closePx decimal.Decimal, reason string,
	realizedPnL decimal.Decimal, exchangePnL, exchangeFee, exchangeClosePx *decimal.Decimal) error {
	r.mu.Lock()
	o, ok := r.realOrders[id]
	// Mirrors the real SQL's "WHERE ... AND closed_at IS NULL": closing is idempotent, so a second
	// close of the same order changes nothing and reports ErrOrderAlreadyClosed. A fake without
	// this guard would let a test pass against a double-close the real repository refuses — which
	// is exactly the bug that overwrote real order 38's outcome.
	if ok && o.ClosedAt != nil {
		r.mu.Unlock()
		return fmt.Errorf("real order %d: %w", id, port.ErrOrderAlreadyClosed)
	}
	if ok {
		o.ExchangeRealizedPnL = exchangePnL
		o.ExchangeFee = exchangeFee
		o.ExchangeClosePx = exchangeClosePx
		// Mirrors the real SQL: settle to 'filled' once the exchange confirms. A fake that skipped
		// this would let a test pass against behavior the real repository does not have.
		o.Status = "filled"
		o.LastError, o.LastErrorAt = nil, nil
		r.realOrders[id] = o
	}
	r.mu.Unlock()
	return r.CloseRealOrder(ctx, id, closePx, reason, realizedPnL)
}

func (r *fakeRepository) SetRealOrderExchangeRaw(ctx context.Context, id int64, leg string, raw json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return nil
	}
	switch leg {
	case "open":
		o.ExchangeOpenRaw = raw
	case "close":
		o.ExchangeCloseRaw = raw
	default:
		return fmt.Errorf("unknown order leg %q", leg)
	}
	r.realOrders[id] = o
	return nil
}

func (r *fakeRepository) SetRealOrderError(ctx context.Context, id int64, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return nil
	}
	now := time.Now()
	o.LastError, o.LastErrorAt = &message, &now
	r.realOrders[id] = o
	return nil
}

func (r *fakeRepository) ClearRealOrderError(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok {
		return nil
	}
	o.LastError, o.LastErrorAt = nil, nil
	r.realOrders[id] = o
	return nil
}

func (r *fakeRepository) UpdateRealOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.realOrders[id]
	if !ok || o.ClosedAt != nil {
		return nil
	}
	if maxPct.GreaterThan(o.PnLMaxPct) {
		o.PnLMaxPct = maxPct
	}
	if minPct.LessThan(o.PnLMinPct) {
		o.PnLMinPct = minPct
	}
	r.realOrders[id] = o
	return nil
}
func (r *fakeRepository) ListRealPositions(ctx context.Context, f port.PositionFilter) ([]port.RealOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.RealOrder
	for _, o := range r.realOrders {
		if f.InstID != "" && o.InstID != f.InstID {
			continue
		}
		if f.Open != nil {
			// Mirrors internal/postgres's own query, which includes 'closing' — an order whose
			// flatten is in flight is still an open position on the exchange. The fake omitted it,
			// which quietly hid a state the production code genuinely encounters (a fake that
			// diverges from its real counterpart weakens every test using it, CLAUDE.md §17).
			isOpenPosition := o.ClosedAt == nil &&
				(o.Status == "filled" || o.Status == "partial" || o.Status == "closing")
			if *f.Open {
				if !isOpenPosition {
					continue
				}
			} else {
				if isOpenPosition {
					continue
				}
			}
		}
		out = append(out, o)
	}
	if f.Limit > 0 && f.Offset < len(out) {
		end := f.Offset + f.Limit
		if end > len(out) {
			end = len(out)
		}
		out = out[f.Offset:end]
	}
	return out, nil
}
func (r *fakeRepository) CountRealPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	out, err := r.ListRealPositions(ctx, f)
	return len(out), err
}
func (r *fakeRepository) RecordRealOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal, source string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextRealID++
	r.realOrderAdjustments = append(r.realOrderAdjustments, port.PaperOrderAdjustment{
		ID: r.nextRealID, OrderID: orderID, Field: field, OldValue: oldValue, NewValue: newValue, Source: source,
	})
	return nil
}
func (r *fakeRepository) ListRealOrderAdjustments(ctx context.Context, orderID int64) ([]port.PaperOrderAdjustment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.PaperOrderAdjustment
	for _, a := range r.realOrderAdjustments {
		if a.OrderID == orderID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *fakeRepository) GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ae, ok := r.accounts[mode]; ok {
		return ae, nil
	}
	ae := port.AccountEquity{Mode: mode, InitialUSD: initialUSD, EquityUSD: initialUSD, AccountBalanceUSD: initialUSD}
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
	ae.AccountBalanceUSD = ae.AccountBalanceUSD.Add(pnl)
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

func (r *fakeRepository) RecordExchangeBalance(ctx context.Context, mode string, rawBalanceUSD, safeMoneyUSD decimal.Decimal, instID string) (port.AccountEquity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Counted because the real implementation opens a TRANSACTION per call against one shared
	// account row: a redundant call is invisible in the resulting data (the second sees a zero
	// delta and writes no history row) but is real database work, so the count is the only way to
	// observe it.
	r.recordExchangeBalanceCalls++
	ae, ok := r.accounts[mode]
	if !ok {
		return port.AccountEquity{}, fmt.Errorf("record exchange balance: no account row for mode %s", mode)
	}
	delta := rawBalanceUSD.Sub(ae.AccountBalanceUSD)
	ae.AccountBalanceUSD = rawBalanceUSD
	tradable := rawBalanceUSD.Sub(safeMoneyUSD)
	if tradable.IsNegative() {
		tradable = decimal.Zero
	}
	ae.EquityUSD = tradable
	if !delta.IsZero() {
		r.equityPoints = append(r.equityPoints, port.EquityPoint{
			Mode: mode, EquityUSD: ae.EquityUSD, DeltaUSD: delta, Reason: "trade", InstID: instID,
		})
	}
	r.accounts[mode] = ae
	return ae, nil
}

func (r *fakeRepository) SetAccountCap(ctx context.Context, mode string, newCapUSD decimal.Decimal) (port.AccountEquity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.accounts[mode].EquityUSD
	ae := port.AccountEquity{
		Mode:       mode,
		InitialUSD: newCapUSD,
		EquityUSD:  newCapUSD,
		// AccountBalanceUSD moves to newCapUSD too (CLAUDE.md §31.3 correction): choosing a cap is
		// economically a deposit/withdrawal, which changes the real balance by construction — not
		// an independent re-baselining of EquityUSD that would leave Balance < Equity with a gap
		// no real trade produced.
		AccountBalanceUSD: newCapUSD,
		ResetCount:        r.accounts[mode].ResetCount + 1,
	}
	r.accounts[mode] = ae
	r.equityPoints = append(r.equityPoints, port.EquityPoint{
		Mode: mode, EquityUSD: newCapUSD, DeltaUSD: newCapUSD.Sub(previous), Reason: "reset",
	})
	return ae, nil
}

// SetTradingCap mirrors the real implementation: sets ONLY the cap and the derived equity
// (clamped to the real balance), never AccountBalanceUSD — a fake that rewrote the balance here
// would let a test pass against behavior the real repository deliberately forbids in real mode.
func (r *fakeRepository) SetTradingCap(ctx context.Context, mode string, capUSD decimal.Decimal) (port.AccountEquity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ae := r.accounts[mode]
	previous := ae.EquityUSD
	equity := capUSD
	if ae.AccountBalanceUSD.LessThan(equity) {
		equity = ae.AccountBalanceUSD
	}
	ae.Mode = mode
	ae.TradingCapUSD = &capUSD
	ae.EquityUSD = equity
	r.accounts[mode] = ae
	r.equityPoints = append(r.equityPoints, port.EquityPoint{
		Mode: mode, EquityUSD: equity, DeltaUSD: equity.Sub(previous), Reason: "cap",
	})
	return ae, nil
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
		Repo: repo,
		// Shared-account defaults (CLAUDE.md §15.6): a $1000 pool with the production caps, so
		// sizing tests exercise the real cap arithmetic rather than an unbounded path.
		// ActiveTokenCount: 10 keeps dynamicNotional's $1000/10 = $100 result identical to the old
		// fixed NotionalUSD: dec("100") this replaced (CLAUDE.md §31.2), so existing tests that
		// assert a $100 order size don't need to change just because sizing became dynamic.
		Mode:                "paper",
		AccountInitialUSD:   dec("1000"),
		ActiveTokenCount:    10,
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

// TestTradingFee_ChargedOnNotionalBothLegs verifies the fee formula against the exact real-money
// reasoning behind it: OKX charges a percentage of NOTIONAL (size * leverage), not of the margin
// committed (size alone), on BOTH the entry and exit market order. Values chosen so entry and exit
// notional differ (price moved), which is what would expose a bug that charged the fee only once
// or against the wrong base.
func TestTradingFee_ChargedOnNotionalBothLegs(t *testing.T) {
	old := TakerFeeRate
	TakerFeeRate = dec("0.0005") // OKX's real base-tier taker fee, 0.05%
	defer func() { TakerFeeRate = old }()

	// size=10 (margin), leverage=10 -> entry notional = 100. Price moves from 100 to 110 -> exit
	// notional = 100 * 110/100 = 110.
	order := port.PaperOrder{Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("10")}
	fee := tradingFee(order, dec("110"))
	want := dec("100").Add(dec("110")).Mul(dec("0.0005")) // (100+110)*0.0005 = 0.105
	if !fee.Equal(want) {
		t.Errorf("expected fee %s (0.05%% of entry+exit notional), got %s", want, fee)
	}
}

// TestTradingFee_ZeroRateChargesNothing confirms the feature is genuinely additive: a zero
// TakerFeeRate (the Go zero value, and every existing test's implicit default before this feature
// existed) must reproduce the exact pre-2026-09-06 PnL with no fee line at all.
func TestTradingFee_ZeroRateChargesNothing(t *testing.T) {
	old := TakerFeeRate
	TakerFeeRate = decimal.Zero
	defer func() { TakerFeeRate = old }()

	order := port.PaperOrder{Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("10")}
	if fee := tradingFee(order, dec("110")); !fee.IsZero() {
		t.Errorf("expected zero fee when TakerFeeRate is zero, got %s", fee)
	}
}

// TestRealizedPnL_DeductsFee ties the two together: realizedPnL's net result must equal the
// pre-fee gross move minus the fee tradingFee computes independently, for both a winning and a
// losing trade — a fee must reduce a win and DEEPEN a loss, never the reverse.
func TestRealizedPnL_DeductsFee(t *testing.T) {
	old := TakerFeeRate
	TakerFeeRate = dec("0.0005")
	defer func() { TakerFeeRate = old }()

	order := port.PaperOrder{Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("10")}

	winClose := dec("110")
	gross := grossPnL(order, winClose)
	fee := tradingFee(order, winClose)
	net := realizedPnL(order, winClose)
	if !net.Equal(gross.Sub(fee)) {
		t.Errorf("winning trade: net %s != gross %s - fee %s", net, gross, fee)
	}
	if !net.LessThan(gross) {
		t.Errorf("a fee must reduce a winning trade's PnL: net %s should be < gross %s", net, gross)
	}

	lossClose := dec("90")
	grossLoss := grossPnL(order, lossClose)
	netLoss := realizedPnL(order, lossClose)
	if !netLoss.LessThan(grossLoss) {
		t.Errorf("a fee must DEEPEN a losing trade, never reduce the loss: net %s should be < gross %s", netLoss, grossLoss)
	}
}

// TestRealizedPnLWithFunding_LongPaysPositiveRate covers OKX's own sign convention (a positive
// fundingRate means longs pay shorts): a long position accruing a positive summed rate must have
// that amount SUBTRACTED from its PnL, and the returned fundingCost must be positive (a cost).
func TestRealizedPnLWithFunding_LongPaysPositiveRate(t *testing.T) {
	repo := newFakeRepository()
	opened := time.Now().Add(-time.Hour)
	closed := time.Now()
	repo.fundingRates = []port.FundingRate{
		{InstID: "BTC", FundingTime: opened.Add(30 * time.Minute), FundingRate: dec("0.0001")},
	}
	order := port.PaperOrder{
		InstID: "BTC", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("10"), OpenedAt: opened,
	}
	pnl, fundingCost := realizedPnLWithFunding(context.Background(), repo, order, dec("100"), closed, testLogger())
	// notional = 10*10 = 100; funding = 0.0001*100 = 0.01, a cost to the long.
	if !fundingCost.Equal(dec("0.01")) {
		t.Errorf("expected fundingCost=0.01 (a cost), got %s", fundingCost)
	}
	if !pnl.Equal(dec("-0.01")) {
		t.Errorf("expected pnl=-0.01 (round-trip price, only funding cost applied), got %s", pnl)
	}
}

// TestRealizedPnLWithFunding_ShortReceivesPositiveRate mirrors the above for a short: a positive
// summed rate is a CREDIT to a short (longs are paying, and a short is on the other side of that
// payment), so it must be ADDED to PnL and reported as a negative fundingCost.
func TestRealizedPnLWithFunding_ShortReceivesPositiveRate(t *testing.T) {
	repo := newFakeRepository()
	opened := time.Now().Add(-time.Hour)
	closed := time.Now()
	repo.fundingRates = []port.FundingRate{
		{InstID: "BTC", FundingTime: opened.Add(30 * time.Minute), FundingRate: dec("0.0001")},
	}
	order := port.PaperOrder{
		InstID: "BTC", Side: "sell", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("10"), OpenedAt: opened,
	}
	pnl, fundingCost := realizedPnLWithFunding(context.Background(), repo, order, dec("100"), closed, testLogger())
	if !fundingCost.Equal(dec("-0.01")) {
		t.Errorf("expected fundingCost=-0.01 (a credit, negative), got %s", fundingCost)
	}
	if !pnl.Equal(dec("0.01")) {
		t.Errorf("expected pnl=+0.01 (a short receiving the credit), got %s", pnl)
	}
}

// TestRealizedPnLWithFunding_OutsideWindowNotCounted confirms a funding period that settled
// before the position opened or after it closed is excluded — the sum must be scoped to exactly
// the position's own lifetime, not to every rate ever polled for the instrument.
func TestRealizedPnLWithFunding_OutsideWindowNotCounted(t *testing.T) {
	repo := newFakeRepository()
	opened := time.Now().Add(-time.Hour)
	closed := time.Now()
	repo.fundingRates = []port.FundingRate{
		{InstID: "BTC", FundingTime: opened.Add(-time.Minute), FundingRate: dec("0.01")}, // before open
		{InstID: "BTC", FundingTime: closed.Add(time.Minute), FundingRate: dec("0.01")},  // after close
	}
	order := port.PaperOrder{
		InstID: "BTC", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("10"), OpenedAt: opened,
	}
	_, fundingCost := realizedPnLWithFunding(context.Background(), repo, order, dec("100"), closed, testLogger())
	if !fundingCost.IsZero() {
		t.Errorf("expected zero funding cost when every settled rate falls outside the position's lifetime, got %s", fundingCost)
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

func TestRunUpdates_AppliesAdjustmentInPlace(t *testing.T) {
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

	// The first call only establishes the update cadence baseline (CLAUDE.md's 2026-09-03 fix —
	// an order's first-ever update check no longer fires on sight).
	pt.runUpdates(context.Background(), "1m", dec("100"), testLogger())
	pt.runUpdates(context.Background(), "1m", dec("105"), testLogger())

	if model.calls == 0 {
		t.Fatalf("expected Predict to be called")
	}

	orders, err := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("list open orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected the adjustment to edit the existing order in place, not create a new one — got %d open orders", len(orders))
	}
	edited := orders[0]
	if edited.ID != id {
		t.Fatalf("expected the same order id %d to remain, got %d", id, edited.ID)
	}
	// SL should have tightened (moved up from 95, toward locking profit at price 105).
	if !edited.SLPx.GreaterThan(sl) {
		t.Errorf("expected SL to have tightened above %s, got %s", sl, edited.SLPx)
	}

	adjustments, err := repo.ListPaperOrderAdjustments(context.Background(), id)
	if err != nil {
		t.Fatalf("list adjustments: %v", err)
	}
	if len(adjustments) == 0 {
		t.Fatalf("expected at least one adjustment row to be recorded")
	}
	for _, a := range adjustments {
		if a.Source != "model" {
			t.Errorf("expected adjustment source=model, got %q", a.Source)
		}
	}
}

func TestRunUpdates_RepeatedAdjustmentsAllApplyToSameOrder(t *testing.T) {
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

	// Three adjustment rounds, each proposing a tighter stop than the last — all must apply to the
	// same order id, never spawn a new one (CLAUDE.md's 2026-09-02 revision away from shadow forks).
	for i, proposed := range []string{"97", "99", "101"} {
		model.action = domain.Action{Action: domain.ActionUpdate, SLPx: dec(proposed), TPPx: dec("108")}
		pt.lifecycle = nil // clear the conductor's per-order update cadence so each round fires
		pt.conductorOnce = sync.Once{}
		// The first call after resetting the conductor only establishes that round's baseline
		// (CLAUDE.md's 2026-09-03 fix); the second is what actually reaches the model.
		pt.runUpdates(context.Background(), "1m", dec("100"), testLogger())
		pt.runUpdates(context.Background(), "1m", dec("105"), testLogger())

		orders, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
		if len(orders) != 1 {
			t.Fatalf("round %d: expected exactly 1 open order, got %d", i+1, len(orders))
		}
		if orders[0].ID != baselineID {
			t.Fatalf("round %d: expected order id to stay %d, got %d", i+1, baselineID, orders[0].ID)
		}
	}

	adjustments, err := repo.ListPaperOrderAdjustments(context.Background(), baselineID)
	if err != nil {
		t.Fatalf("list adjustments: %v", err)
	}
	if len(adjustments) < 3 {
		t.Errorf("expected at least 3 recorded SL adjustment rows (one per round), got %d", len(adjustments))
	}
}

func TestRunUpdates_NoOpActionDoesNotChangeOrder(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	pt.Model = &fakeModelClientRL{action: domain.Action{}} // zero adjustment, matches the no-op fail-safe

	sl := dec("95")
	id, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
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
		t.Errorf("expected no new order for a zero-adjustment action, got %d open orders", len(orders))
	}

	adjustments, err := repo.ListPaperOrderAdjustments(context.Background(), id)
	if err != nil {
		t.Fatalf("list adjustments: %v", err)
	}
	if len(adjustments) != 0 {
		t.Errorf("expected no adjustment rows recorded for a zero-adjustment action, got %d", len(adjustments))
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

// TestPaperOrderAdjustments_RecordAndListRoundTrip covers the fake's implementation of the
// adjustment-log methods directly (CLAUDE.md §15.4/§15.12 revision, 2026-09-02) — the repository
// contract TestRunUpdates_AppliesAdjustmentInPlace exercises indirectly through the lifecycle.
func TestPaperOrderAdjustments_RecordAndListRoundTrip(t *testing.T) {
	repo := newFakeRepository()
	ctx := context.Background()

	orderID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100")})
	otherID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "ETH-USDT-SWAP", Side: "buy", EntryPx: dec("100")})

	old, new1 := ptr(dec("95")), ptr(dec("97"))
	if err := repo.RecordPaperOrderAdjustment(ctx, orderID, "sl", old, new1, "model"); err != nil {
		t.Fatalf("record sl adjustment: %v", err)
	}
	old2, new2 := ptr(dec("110")), ptr(dec("108"))
	if err := repo.RecordPaperOrderAdjustment(ctx, orderID, "tp", old2, new2, "model"); err != nil {
		t.Fatalf("record tp adjustment: %v", err)
	}
	// An adjustment on an unrelated order must not show up in orderID's history.
	if err := repo.RecordPaperOrderAdjustment(ctx, otherID, "sl", ptr(dec("50")), ptr(dec("52")), "model"); err != nil {
		t.Fatalf("record unrelated adjustment: %v", err)
	}

	adjustments, err := repo.ListPaperOrderAdjustments(ctx, orderID)
	if err != nil {
		t.Fatalf("list adjustments: %v", err)
	}
	if len(adjustments) != 2 {
		t.Fatalf("expected exactly 2 adjustments for orderID, got %d", len(adjustments))
	}
	if adjustments[0].Field != "sl" || !adjustments[0].NewValue.Equal(dec("97")) {
		t.Errorf("expected first adjustment field=sl new=97, got field=%s new=%v", adjustments[0].Field, adjustments[0].NewValue)
	}
	if adjustments[1].Field != "tp" || !adjustments[1].NewValue.Equal(dec("108")) {
		t.Errorf("expected second adjustment field=tp new=108, got field=%s new=%v", adjustments[1].Field, adjustments[1].NewValue)
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

// CLAUDE.md §31.2: with RLSizing off, a new order's fixed-path size tracks the account's CURRENT
// equity divided by ActiveTokenCount — not a config constant — so it behaves like a real exchange
// account (grows after a win, shrinks after a loss) rather than staying pinned to whatever number
// was configured when the service last started.
func TestEvaluateStrategies_DynamicSizingTracksCurrentEquity(t *testing.T) {
	repo := newFakeRepository()
	pt := newSizingTestPaperTrader(repo, nil) // RLSizing left false
	pt.ActiveTokenCount = 5
	// Pre-seed a DIFFERENT equity than newTestPaperTrader's AccountInitialUSD default (1000), so a
	// pass proves the live value is actually read, not the configured fallback.
	repo.accounts["paper"] = port.AccountEquity{Mode: "paper", InitialUSD: dec("1000"), EquityUSD: dec("250")}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}

	o := openedOrder(t, repo)
	if !o.Size.Equal(dec("50")) {
		t.Errorf("expected size = current equity 250 / 5 active tokens = 50, got %s", o.Size)
	}
}

// The same account, but fewer active tokens (e.g. one was disabled) — the divisor changes and so
// must the resulting size, proving ActiveTokenCount is actually load-bearing and not just read
// once at construction and ignored.
func TestEvaluateStrategies_DynamicSizingTracksActiveTokenCount(t *testing.T) {
	repo := newFakeRepository()
	pt := newSizingTestPaperTrader(repo, nil)
	pt.ActiveTokenCount = 2
	repo.accounts["paper"] = port.AccountEquity{Mode: "paper", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}

	o := openedOrder(t, repo)
	if !o.Size.Equal(dec("500")) {
		t.Errorf("expected size = 1000 / 2 active tokens = 500, got %s", o.Size)
	}
}

// A drained (zero/negative) or unreadable equity must fall back to a sane magnitude
// (AccountInitialUSD/ActiveTokenCount) rather than opening at ~$0, which would round-trip through
// every downstream percentage/leverage calculation as noise.
func TestEvaluateStrategies_DynamicSizingFallsBackWhenEquityNotPositive(t *testing.T) {
	repo := newFakeRepository()
	pt := newSizingTestPaperTrader(repo, nil)
	pt.ActiveTokenCount = 10
	repo.accounts["paper"] = port.AccountEquity{Mode: "paper", InitialUSD: dec("1000"), EquityUSD: dec("0")}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}

	o := openedOrder(t, repo)
	if !o.Size.Equal(dec("100")) {
		t.Errorf("expected fallback to AccountInitialUSD(1000)/ActiveTokenCount(10) = 100, got %s", o.Size)
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

// CLAUDE.md §31.2: AccountBalanceUSD (the real, continuous running total) must track the exact
// same trade PnL as EquityUSD ("Total Equity" — the balance since the last chosen baseline) —
// they only diverge once a reset happens, which is the whole point of having two fields.
func TestApplyRealizedPnL_UpdatesAccountBalanceAlongsideEquity(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "paper", dec("40")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	acct, _, err := repo.ApplyRealizedPnL(ctx, "paper", dec("-5"), nil, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("apply pnl: %v", err)
	}
	if !acct.EquityUSD.Equal(dec("35")) {
		t.Errorf("EquityUSD: want 35, got %s", acct.EquityUSD)
	}
	if !acct.AccountBalanceUSD.Equal(dec("35")) {
		t.Errorf("AccountBalanceUSD: want 35, got %s", acct.AccountBalanceUSD)
	}
}

// The defect this whole redesign exists to fix: before AccountBalanceUSD existed, EquityUSD was
// the only running total, so re-baselining it (a reset, whether automatic or operator-triggered)
// destroyed the real cumulative history. AccountBalanceUSD must survive a SetAccountCap untouched.
// CLAUDE.md §31.3: SetAccountCap is economically a deposit/withdrawal, so it must move
// EquityUSD AND AccountBalanceUSD to the SAME new value together — never leave a gap between them
// that no real trade produced. Since neither field in this schema ever carries unrealized PnL
// (both only move on ApplyRealizedPnL, at a position's close), Balance and Equity are mathematically
// required to stay equal outside of realized-PnL deltas; a SetAccountCap that only touched one of
// them (the original, buggy version of this method) left Balance permanently below Equity for no
// trading reason at all — exactly the defect a real operator caught live (Balance $6.96 vs Equity
// $39.66 after setting a $40 cap against a $7.30 real balance).
func TestSetAccountCap_MovesBalanceAndEquityTogether(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "paper", dec("100")); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// Drift EquityUSD and AccountBalanceUSD together away from the seed, the way real trading
	// would, before the operator chooses a new cap.
	if _, _, err := repo.ApplyRealizedPnL(ctx, "paper", dec("-49"), nil, "ETH-USDT-SWAP"); err != nil {
		t.Fatalf("apply pnl: %v", err)
	}

	acct, err := repo.SetAccountCap(ctx, "paper", dec("40"))
	if err != nil {
		t.Fatalf("set account cap: %v", err)
	}
	if !acct.EquityUSD.Equal(dec("40")) {
		t.Errorf("EquityUSD should re-baseline to the new cap: want 40, got %s", acct.EquityUSD)
	}
	if !acct.InitialUSD.Equal(dec("40")) {
		t.Errorf("InitialUSD should re-baseline to the new cap: want 40, got %s", acct.InitialUSD)
	}
	// A cap change is a deposit/withdrawal: it changes the real balance too, to the SAME value —
	// regardless of what the real balance was before (51, here), never a value derived from it.
	if !acct.AccountBalanceUSD.Equal(dec("40")) {
		t.Errorf("AccountBalanceUSD must move to the new cap alongside EquityUSD: want 40, got %s — "+
			"leaving it at the old value creates a gap between Balance and Equity that no real "+
			"trade produced, which is mathematically incoherent in a schema where neither field "+
			"ever carries unrealized PnL", acct.AccountBalanceUSD)
	}
	if !acct.EquityUSD.Equal(acct.AccountBalanceUSD) {
		t.Errorf("invariant violated: EquityUSD (%s) must equal AccountBalanceUSD (%s) immediately "+
			"after a cap change, since no PnL has been realized since", acct.EquityUSD, acct.AccountBalanceUSD)
	}
	if acct.ResetCount != 1 {
		t.Errorf("ResetCount: want 1, got %d", acct.ResetCount)
	}

	// And both fields keep moving together afterward, from the SAME new baseline.
	acct, _, err = repo.ApplyRealizedPnL(ctx, "paper", dec("10"), nil, "SOL-USDT-SWAP")
	if err != nil {
		t.Fatalf("apply pnl after cap: %v", err)
	}
	if !acct.EquityUSD.Equal(dec("50")) {
		t.Errorf("EquityUSD after +10: want 50 (40+10), got %s", acct.EquityUSD)
	}
	if !acct.AccountBalanceUSD.Equal(dec("50")) {
		t.Errorf("AccountBalanceUSD after +10: want 50 (40+10, same baseline as Equity), got %s", acct.AccountBalanceUSD)
	}
	if !acct.EquityUSD.Equal(acct.AccountBalanceUSD) {
		t.Errorf("invariant violated after a trade: EquityUSD (%s) must still equal AccountBalanceUSD (%s)", acct.EquityUSD, acct.AccountBalanceUSD)
	}
}

// TestRecordExchangeBalance_ReservedNeverReportsAsATrade is the CLAUDE.md §32 incident,
// reproduced exactly: a real account seeded at $40 (no SafeMoneyUSD reserve applied yet), then the
// FIRST poll after safe_money_usd=20 is configured reports the exchange's raw balance completely
// unchanged at $40. Before this fix, RealTrader.recordEquityReal computed
// tradableEquity($40) = $20 FIRST and fed that into ApplyRealizedPnL's delta-from-EquityUSD
// comparison — which read as a genuine $20 trade loss and dragged the real AccountBalanceUSD down
// to $20 too, even though the exchange balance never moved. The fix must show AccountBalanceUSD
// staying at $40 (the exchange's own truth) while EquityUSD alone reflects the $20 reserve split.
func TestRecordExchangeBalance_ReservedNeverReportsAsATrade(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "real", dec("40")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// Raw exchange balance is unchanged at $40; a $20 safe-money reserve is applied for the first
	// time on this poll.
	acct, err := repo.RecordExchangeBalance(ctx, "real", dec("40"), dec("20"), "BTC")
	if err != nil {
		t.Fatalf("record exchange balance: %v", err)
	}
	if !acct.AccountBalanceUSD.Equal(dec("40")) {
		t.Errorf("AccountBalanceUSD must stay at the exchange's real, unchanged balance: want 40, got %s "+
			"— a reserve split must never be misreported as a real trade loss", acct.AccountBalanceUSD)
	}
	if !acct.EquityUSD.Equal(dec("20")) {
		t.Errorf("EquityUSD must reflect the reserve split (40-20): want 20, got %s", acct.EquityUSD)
	}

	// No history point should have been written for a zero-delta raw balance, since nothing about
	// the real account actually changed — only the derived tradable view did.
	points, err := repo.ListEquityHistory(ctx, "real", time.Time{}, 0)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	for _, p := range points {
		if p.Reason == "trade" {
			t.Errorf("no 'trade' history point should exist for a reserve split with zero real balance change, got %+v", p)
		}
	}

	// Now the exchange balance genuinely drops by $5 (a real trade loss) on the next poll — this
	// must show up as a real $5 delta on AccountBalanceUSD, and EquityUSD must track it minus the
	// same $20 reserve.
	acct, err = repo.RecordExchangeBalance(ctx, "real", dec("35"), dec("20"), "BTC")
	if err != nil {
		t.Fatalf("record exchange balance after real loss: %v", err)
	}
	if !acct.AccountBalanceUSD.Equal(dec("35")) {
		t.Errorf("AccountBalanceUSD after a real $5 loss: want 35, got %s", acct.AccountBalanceUSD)
	}
	if !acct.EquityUSD.Equal(dec("15")) {
		t.Errorf("EquityUSD after a real $5 loss (35-20 reserve): want 15, got %s", acct.EquityUSD)
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

// A stop the model has trailed CLOSER than MinSLDistPct must be left alone. tightenOverWideStop
// exists to pull an over-wide stop in, never to push a tight one back out — but it routes through
// Clamps.Apply, which enforces a RANGE, so before this was guarded it dragged every trailed stop
// back to the floor on the very next tick.
//
// Order 2114 (PUMP, 2026-09-06) is the fixture: entry 0.003970, leverage ~8.9, model trailed the
// stop to 0.003965031 (0.125% from entry, locking in profit). MinSLDistPct is 0.5%, so Apply
// rebuilt it at 0.00395015 and stored that instead — on every tick, so the model's stop never
// survived and paper_order_adjustments showed a value that did not match paper_orders.
func TestMonitorOpenOrders_DoesNotWidenAStopTrailedInsideTheMinimum(t *testing.T) {
	repo := newFakeRepository()
	entry := dec("0.003970")
	trailed := dec("0.003965031") // 0.125% from entry — inside MinSLDistPct on purpose
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: entry, SLPx: &trailed,
		Size: dec("3"), Leverage: dec("8.899"), OpenedAt: time.Now(),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.RLClamps = conductor.Clamps{
		MinSLDistPct: dec("0.005"), MaxSLDistPct: dec("0.05"), MaxLossPct: dec("0.15"),
	}

	if err := pt.monitorOpenOrders(context.Background(), dec("0.003980"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	got := repo.orders[id].SLPx
	if got == nil || !got.Equal(trailed) {
		t.Errorf("a stop trailed to %s must survive: got %v — widening it back to the floor "+
			"destroys the locked-in profit the model just secured", trailed, got)
	}
}

// The over-wide direction must still be corrected — this guard must not disable the self-healing
// the function exists for (the 2026-09-01 incident).
func TestMonitorOpenOrders_StillTightensAnOverWideStopAfterTheWideningGuard(t *testing.T) {
	repo := newFakeRepository()
	entry := dec("100")
	wide := dec("95") // 5% at 20x = 100% loss, far past MaxLossPct
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: entry, SLPx: &wide,
		Size: dec("100"), Leverage: dec("20"), OpenedAt: time.Now(),
	})

	pt := newTestPaperTrader(repo, nil)
	pt.RLClamps = conductor.Clamps{
		MinSLDistPct: dec("0.005"), MaxSLDistPct: dec("0.05"), MaxLossPct: dec("0.15"),
	}

	// Priced above the corrected stop so the order stays OPEN and the stored level can be
	// inspected — at price 100 the tightened 99.5 stop is touched on this same tick (which is
	// TestMonitorOpenOrders_TightenedStopAppliesOnTheSameTick's subject) and the order closes.
	if err := pt.monitorOpenOrders(context.Background(), dec("105"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	got := repo.orders[id].SLPx
	if got == nil || !got.GreaterThan(wide) {
		t.Errorf("an over-wide stop must still be tightened up from %s, got %v", wide, got)
	}
}

// The requested real-mode model (2026-09-08), asserted end to end through the fake repository:
// two independent numbers — the exchange's total, and the operator-chosen slice traded with —
// where realized PnL accrues to the slice and the untraded reserve stays put.
//
// The operator's own scenario, verbatim: $40 on the exchange, trade with $20; after +$5 profit
// that reads $25 tradable against $45 total; raising the cap to $30 then takes $5 from the
// reserve, leaving the total at $45.
func TestSetTradingCap_PnLAccruesToCapWhileReserveStaysPut(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "real", dec("40")); err != nil {
		t.Fatalf("seed real account: %v", err)
	}

	ae, err := repo.SetTradingCap(ctx, "real", dec("20"))
	if err != nil {
		t.Fatalf("set trading cap: %v", err)
	}
	if !ae.EquityUSD.Equal(dec("20")) {
		t.Fatalf("tradable equity after cap: want 20, got %s", ae.EquityUSD)
	}
	// The whole point of the real-mode split: the exchange's own total is untouched by a cap.
	if !ae.AccountBalanceUSD.Equal(dec("40")) {
		t.Fatalf("a cap must not move the real balance: want 40, got %s", ae.AccountBalanceUSD)
	}

	// +$5 realized on the exchange. The reserve (40-20=20) is what stays constant, so equity
	// tracks the gain rather than staying pinned at the cap.
	reserve := ae.AccountBalanceUSD.Sub(ae.EquityUSD)
	rawBalance := dec("45")
	wantEquity := rawBalance.Sub(reserve)
	if !wantEquity.Equal(dec("25")) {
		t.Fatalf("derivation wrong: want 25 tradable after +5, got %s", wantEquity)
	}

	// Raising the cap re-splits the SAME total: 5 comes out of the reserve, the total holds at 45.
	repo.mu.Lock()
	repo.accounts["real"] = port.AccountEquity{
		Mode: "real", AccountBalanceUSD: rawBalance, EquityUSD: wantEquity, TradingCapUSD: ae.TradingCapUSD,
	}
	repo.mu.Unlock()

	ae2, err := repo.SetTradingCap(ctx, "real", dec("30"))
	if err != nil {
		t.Fatalf("raise trading cap: %v", err)
	}
	if !ae2.EquityUSD.Equal(dec("30")) {
		t.Fatalf("tradable equity after raise: want 30, got %s", ae2.EquityUSD)
	}
	if !ae2.AccountBalanceUSD.Equal(dec("45")) {
		t.Fatalf("total must hold at 45 across a cap change, got %s", ae2.AccountBalanceUSD)
	}
	if got := ae2.AccountBalanceUSD.Sub(ae2.EquityUSD); !got.Equal(dec("15")) {
		t.Fatalf("reserve after raising cap 20->30: want 15, got %s", got)
	}
}

// A cap above what the account actually holds cannot be honored — sizing against money that isn't
// there is worse than clamping, and the clamp is what keeps EquityUSD <= AccountBalanceUSD an
// invariant rather than a hope.
func TestSetTradingCap_ClampsToRealBalance(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "real", dec("40")); err != nil {
		t.Fatalf("seed real account: %v", err)
	}

	ae, err := repo.SetTradingCap(ctx, "real", dec("999"))
	if err != nil {
		t.Fatalf("set trading cap: %v", err)
	}
	if !ae.EquityUSD.Equal(dec("40")) {
		t.Fatalf("cap above balance must clamp to the balance: want 40, got %s", ae.EquityUSD)
	}
}

// TestMoneyPrecisionStaysBoundedAcrossGenerations reproduces the failure that stopped paper
// trading on 2026-09-11 and proves the rounding fixes it.
//
// Dynamic sizing makes size = equity/activeTokens — a repeating decimal for any roster size that
// isn't a power of ten. Closing that position computes PnL from size, and multiplication ADDS
// digits, so the PnL is longer than the size was. That PnL accumulates into equity, which sizes
// the next order. Nothing bounded the loop: production went 1 digit -> 18 -> 395 -> 4647 -> 11243
// -> 16380, where PostgreSQL's NUMERIC limit rejected every close with SQLSTATE 22P03 and
// positions simply stopped closing.
func TestMoneyPrecisionStaysBoundedAcrossGenerations(t *testing.T) {
	// 9 tokens is what the live roster had, and 40/9 is a repeating decimal.
	const tokens = 9
	equity := dec("40")

	for gen := 1; gen <= 40; gen++ {
		size := equity.Div(decimal.NewFromInt(tokens)).Round(usdScale)
		o := port.PaperOrder{
			Side: "buy", EntryPx: dec("2470.03"), Size: size,
			Leverage: dec("9.6035974025726314"),
		}
		pnl := realizedPnL(o, dec("2455.21"))
		equity = equity.Add(pnl)

		// The bound that matters: a value that keeps growing is the bug, whatever its exact width.
		if got := len(equity.String()); got > 40 {
			t.Fatalf("generation %d: equity reached %d digits — precision is compounding, which is "+
				"what exhausted NUMERIC in production", gen, got)
		}
	}
}

// TestRealizedPnL_IsRoundedToUsdScale pins the specific step that closed the loop: PnL is what
// accumulates into stored equity, so an unrounded value there re-enters the next order's size.
func TestRealizedPnL_IsRoundedToUsdScale(t *testing.T) {
	o := port.PaperOrder{
		Side: "buy",
		// A size with full repeating-decimal precision, exactly as production stored it.
		Size:     dec("1.5513978507918989841083161726078724421648771145478291340375"),
		EntryPx:  dec("2470.03"),
		Leverage: dec("9.6035974025726314"),
	}
	pnl := realizedPnL(o, dec("2455.21"))
	if got := pnl.Exponent(); got < -usdScale {
		t.Errorf("realized pnl carries %d decimal places, want at most %d", -got, usdScale)
	}
}

// TestDynamicNotional_IsRoundedToUsdScale covers the other end — the division that creates the
// repeating decimal in the first place.
func TestDynamicNotional_IsRoundedToUsdScale(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["paper"] = port.AccountEquity{Mode: "paper", InitialUSD: dec("40"), EquityUSD: dec("40")}
	pt := &PaperTrader{
		InstID: "BTC", Repo: repo, Mode: "paper",
		AccountInitialUSD: dec("40"), ActiveTokenCount: 9,
	}

	n := pt.dynamicNotional(context.Background(), testLogger())
	if got := n.Exponent(); got < -usdScale {
		t.Errorf("dynamic notional carries %d decimal places, want at most %d", -got, usdScale)
	}
	if !n.IsPositive() {
		t.Error("expected a positive notional")
	}
}
