package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// fakeRepository's manual-order methods (docs/MANUAL_TRADE_PLAN.md), kept in their own file rather
// than appended to papertrade_test.go's already-large method list — the struct/constructor changes
// live there (Go requires a type's fields in one file), but a self-contained new capability's
// methods don't need to grow that file further.

func (r *fakeRepository) CreateManualOrderIntent(ctx context.Context, in port.ManualOrderIntent) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextIntentID++
	in.ID = r.nextIntentID
	if in.OrderType == "" {
		in.OrderType = "market"
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	r.manualOrderIntents[in.ID] = in
	return in.ID, nil
}

func (r *fakeRepository) ClaimPendingManualOrderIntents(ctx context.Context) ([]port.ManualOrderIntent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.ManualOrderIntent
	for id, in := range r.manualOrderIntents {
		if in.Status != "pending" {
			continue
		}
		in.Status = "claimed"
		r.manualOrderIntents[id] = in
		out = append(out, in)
	}
	return out, nil
}

func (r *fakeRepository) FinishManualOrderIntent(ctx context.Context, id int64, manualOrderID *int64, errMsg *string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	in, ok := r.manualOrderIntents[id]
	if !ok {
		return fmt.Errorf("manual order intent %d not found", id)
	}
	if errMsg != nil {
		in.Status = "failed"
	} else {
		in.Status = "done"
	}
	in.ManualOrderID = manualOrderID
	in.Error = errMsg
	r.manualOrderIntents[id] = in
	return nil
}

func (r *fakeRepository) GetManualOrderIntent(ctx context.Context, id int64) (port.ManualOrderIntent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	in, ok := r.manualOrderIntents[id]
	if !ok {
		return port.ManualOrderIntent{}, fmt.Errorf("manual order intent %d not found", id)
	}
	return in, nil
}

func (r *fakeRepository) OpenManualOrder(ctx context.Context, o port.ManualOrder) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextManualID++
	o.ID = r.nextManualID
	if o.Status == "" {
		o.Status = "pending"
	}
	r.manualOrders[o.ID] = o
	return o.ID, nil
}

func (r *fakeRepository) GetManualOrder(ctx context.Context, id int64) (port.ManualOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok {
		return port.ManualOrder{}, fmt.Errorf("manual order %d not found", id)
	}
	return o, nil
}

func (r *fakeRepository) UpdateManualOrderStatus(ctx context.Context, id int64, status string, entryPx, size, contracts *decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok {
		return fmt.Errorf("manual order %d not found", id)
	}
	wasOpened := o.OpenedAt != nil
	o.Status = status
	if entryPx != nil {
		o.EntryPx = entryPx
	}
	if size != nil {
		o.Size = *size
	}
	if contracts != nil {
		o.Contracts = contracts
	}
	if !wasOpened && (status == "filled" || status == "partial") {
		now := time.Now()
		o.OpenedAt = &now
	}
	r.manualOrders[id] = o
	return nil
}

func (r *fakeRepository) SetManualOrderProtection(ctx context.Context, id int64, algoOrderID *string, protectedByStrategy bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok {
		return fmt.Errorf("manual order %d not found", id)
	}
	o.ExchangeAlgoOrderID = algoOrderID
	o.ProtectedByStrategy = protectedByStrategy
	r.manualOrders[id] = o
	return nil
}

func (r *fakeRepository) CloseManualOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal, exchangeFee *decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok || o.ClosedAt != nil {
		return fmt.Errorf("manual order %d: %w", id, port.ErrOrderAlreadyClosed)
	}
	now := time.Now()
	o.ClosedAt = &now
	o.CloseReason = &reason
	cp := closePx
	o.ClosePx = &cp
	pnl := realizedPnL
	o.RealizedPnL = &pnl
	o.ExchangeFee = exchangeFee
	o.Status = "closed"
	r.manualOrders[id] = o
	return nil
}

func (r *fakeRepository) RequestManualOrderClose(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok || o.ClosedAt != nil {
		return fmt.Errorf("manual order %d is not open", id)
	}
	o.ManualCloseRequested = true
	r.manualOrders[id] = o
	return nil
}

func (r *fakeRepository) CancelManualOrder(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok || o.ClosedAt != nil || o.Status != "resting" {
		return fmt.Errorf("manual order %d is not a resting order", id)
	}
	now := time.Now()
	o.ClosedAt = &now
	reason := "canceled"
	o.CloseReason = &reason
	o.Status = "canceled"
	r.manualOrders[id] = o
	return nil
}

func (r *fakeRepository) SetManualOrderError(ctx context.Context, id int64, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok {
		return fmt.Errorf("manual order %d not found", id)
	}
	o.LastError = &message
	now := time.Now()
	o.LastErrorAt = &now
	r.manualOrders[id] = o
	return nil
}

func (r *fakeRepository) ClearManualOrderError(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.manualOrders[id]
	if !ok {
		return fmt.Errorf("manual order %d not found", id)
	}
	o.LastError = nil
	o.LastErrorAt = nil
	r.manualOrders[id] = o
	return nil
}

func (r *fakeRepository) ListOpenManualOrders(ctx context.Context, instID string) ([]port.ManualOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.ManualOrder
	for _, o := range r.manualOrders {
		if o.InstID == instID && o.ClosedAt == nil && (o.Status == "filled" || o.Status == "partial") {
			out = append(out, o)
		}
	}
	return out, nil
}

func (r *fakeRepository) ListManualOrders(ctx context.Context, f port.PositionFilter) ([]port.ManualOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.ManualOrder
	for _, o := range r.manualOrders {
		if f.InstID != "" && o.InstID != f.InstID {
			continue
		}
		if f.Open != nil {
			isOpen := o.ClosedAt == nil && (o.Status == "filled" || o.Status == "partial")
			if isOpen != *f.Open {
				continue
			}
		}
		out = append(out, o)
	}
	return out, nil
}

func (r *fakeRepository) RecordManualOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextManualID++
	r.manualOrderAdjustments = append(r.manualOrderAdjustments, port.ManualOrderAdjustment{
		ID: r.nextManualID, OrderID: orderID, Field: field, OldValue: oldValue, NewValue: newValue,
	})
	return nil
}

func (r *fakeRepository) ListManualOrderAdjustments(ctx context.Context, orderID int64) ([]port.ManualOrderAdjustment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.ManualOrderAdjustment
	for _, a := range r.manualOrderAdjustments {
		if a.OrderID == orderID {
			out = append(out, a)
		}
	}
	return out, nil
}
