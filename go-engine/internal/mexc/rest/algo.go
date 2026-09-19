package rest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/shopspring/decimal"
)

// Exchange-side stop-loss / take-profit for MEXC.
//
// WHY THIS MATTERS (CLAUDE.md §35): protection must rest on the EXCHANGE, not only in this
// process's tick monitor. A crash, deploy, OOM, stalled consumer or network partition leaves real
// capital running with no stop otherwise — and every one of those has happened to this project at
// least once. The exchange's order survives all of them.
//
// MEXC's model differs from OKX's in a way that shapes this whole file. OKX places a separate
// conditional ORDER carrying both trigger prices, returns an algoId, and cancels the untriggered
// half automatically (OCO). MEXC instead attaches stop/target to an existing POSITION via
// /api/v1/private/stoporder/change_price, keyed by positionId.
//
// The consequences, all handled explicitly below rather than papered over:
//
//   - There is no "algo order id" to return. This adapter returns the POSITION id as the AlgoID,
//     which is what the domain stores and later passes back to amend/cancel/get. It is an opaque
//     handle to every caller, so using a position id is honest rather than a fiction — and it is
//     the only stable identifier MEXC offers for "the protection on this position".
//   - Protection cannot be placed before a position exists. BotTrader already places protection
//     immediately AFTER the entry fills (§35.2), so this ordering is satisfied; but a caller that
//     tried to pre-place would get a clear error rather than a silent no-op.
//   - There is no separate OCO to cancel. Removing protection means clearing the trigger prices.
type stopOrderChangeReq struct {
	// OrderID is MEXC's stop-order id when amending an existing one; PositionID targets the
	// position's attached protection.
	PositionID   int64  `json:"positionId,omitempty"`
	StopLossPx   string `json:"stopLossPrice,omitempty"`
	TakeProfitPx string `json:"takeProfitPrice,omitempty"`
}

// PlaceAlgoOrder attaches stop-loss / take-profit to the position on instID.
//
// Returns the position id as the opaque protection handle (see the file comment for why).
func (c *Client) PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error) {
	if !req.SLTriggerPx.IsPositive() && !req.TPTriggerPx.IsPositive() {
		// Refusing is deliberate. Silently succeeding with neither level set would report a
		// position as protected when nothing protects it — the precise illusion §35 exists to
		// remove.
		return "", fmt.Errorf("mexc place algo order %s: neither stop nor target set", req.InstID)
	}

	posID, err := c.positionIDFor(req.InstID, req.PosSide)
	if err != nil {
		return "", fmt.Errorf("mexc place algo order %s: %w", req.InstID, err)
	}

	body := stopOrderChangeReq{PositionID: posID}
	if req.SLTriggerPx.IsPositive() {
		body.StopLossPx = req.SLTriggerPx.String()
	}
	if req.TPTriggerPx.IsPositive() {
		body.TakeProfitPx = req.TPTriggerPx.String()
	}
	if err := c.do("POST", "/api/v1/private/stoporder/change_price", nil, body, nil); err != nil {
		return "", fmt.Errorf("mexc place algo order %s: %w", req.InstID, err)
	}
	return fmt.Sprintf("%d", posID), nil
}

// AmendAlgoOrder moves an existing protection's trigger prices.
//
// Same endpoint as placing: on MEXC, attaching and moving protection are the same operation
// against the same position. The domain treats them separately because OKX does.
func (c *Client) AmendAlgoOrder(req domain.AlgoOrderAmend) error {
	posID, err := parsePositionID(req.AlgoID)
	if err != nil {
		return fmt.Errorf("mexc amend algo order: %w", err)
	}
	body := stopOrderChangeReq{PositionID: posID}
	if req.SLTriggerPx.IsPositive() {
		body.StopLossPx = req.SLTriggerPx.String()
	}
	if req.TPTriggerPx.IsPositive() {
		body.TakeProfitPx = req.TPTriggerPx.String()
	}
	if body.StopLossPx == "" && body.TakeProfitPx == "" {
		return fmt.Errorf("mexc amend algo order %s: neither stop nor target set", req.AlgoID)
	}
	if err := c.do("POST", "/api/v1/private/stoporder/change_price", nil, body, nil); err != nil {
		return fmt.Errorf("mexc amend algo order %s: %w", req.AlgoID, err)
	}
	return nil
}

// CancelAlgoOrder removes the protection attached to a position.
//
// MEXC has no "cancel this OCO" call, so protection is cleared by cancelling the position's stop
// orders. This is called whenever the protected position is closed by another route, so a flattened
// position cannot leave a live protective order behind that would later open a NEW position in the
// opposite direction (§35.2).
func (c *Client) CancelAlgoOrder(instID, algoID string) error {
	posID, err := parsePositionID(algoID)
	if err != nil {
		return fmt.Errorf("mexc cancel algo order: %w", err)
	}
	body := []map[string]any{{"positionId": posID}}
	if err := c.do("POST", "/api/v1/private/stoporder/cancel", nil, body, nil); err != nil {
		return fmt.Errorf("mexc cancel algo order %s: %w", algoID, err)
	}
	return nil
}

// stopOrderResp is one entry of the stop-order list.
type stopOrderResp struct {
	ID           json.Number `json:"id"`
	PositionID   json.Number `json:"positionId"`
	Symbol       string      `json:"symbol"`
	StopLossPx   json.Number `json:"stopLossPrice"`
	TakeProfitPx json.Number `json:"takeProfitPrice"`
	State        int         `json:"state"`
	// OrderID is the ordinary order a triggered stop created to flatten the position — the source
	// of the true fill price, realized PnL and fee (§37: without it an exchange-executed stop was
	// being recorded as a manual close at the entry price, understating a real loss ~15x).
	OrderID json.Number `json:"orderId"`
}

// MEXC stop-order states: 1 untriggered, 2 cancelled, 3 executed, 4 invalid, 5 execution failed.
const (
	stopStateUntriggered = 1
	stopStateCancelled   = 2
	stopStateExecuted    = 3
	stopStateInvalid     = 4
	stopStateFailed      = 5
)

// GetAlgoOrder reports whether protection is still resting on the exchange.
//
// The exchange is PRIMARY for SL/TP, so this is the check that keeps that trust honest rather than
// assuming a once-successful placement stays valid forever (§35.2). An unreadable status must be
// reported as an error, never as "absent": §35.2 is explicit that unknown is not absent, and
// re-placing protection on an unreadable status would risk two protective orders on one position.
func (c *Client) GetAlgoOrder(instID, algoID string) (domain.AlgoOrderStatus, error) {
	posID, err := parsePositionID(algoID)
	if err != nil {
		return domain.AlgoOrderStatus{}, fmt.Errorf("mexc get algo order: %w", err)
	}

	var resp []stopOrderResp
	params := map[string]string{"symbol": instID, "page_num": "1", "page_size": "50"}
	if err := c.do("GET", "/api/v1/private/stoporder/list/orders", params, nil, &resp); err != nil {
		return domain.AlgoOrderStatus{}, fmt.Errorf("mexc get algo order %s: %w", algoID, err)
	}

	for _, o := range resp {
		if o.PositionID.String() != fmt.Sprintf("%d", posID) {
			continue
		}
		state, actualSide := algoState(o)
		return domain.AlgoOrderStatus{
			AlgoID:      algoID,
			InstID:      o.Symbol,
			State:       state,
			SLTriggerPx: num(o.StopLossPx),
			TPTriggerPx: num(o.TakeProfitPx),
			ActualSide:  actualSide,
			OrdID:       o.OrderID.String(),
		}, nil
	}

	// Not found means no protection rests for this position. Reported as an empty state, which is
	// the domain's own "absent" encoding — distinct from the error path above, which means "could
	// not determine".
	return domain.AlgoOrderStatus{AlgoID: algoID, InstID: instID}, nil
}

// algoState maps MEXC's stop-order state onto the domain vocabulary, and reports which side fired.
//
// ActualSide is what tells this system WHY a position closed. §37 records that without it, a
// stop-loss the exchange had executed was recorded as a manual close priced at entry — making a
// -0.265 loss read as -0.018, and feeding that wrong number to the model as its reward.
//
// MEXC does not report an explicit "which side fired" flag, so it is inferred from which trigger
// price is set on an executed order. When both are set the answer is genuinely ambiguous from this
// endpoint alone; empty is returned rather than a guess, and callers fall back to their own close
// accounting (§37's documented fallback) instead of recording a fabricated reason.
func algoState(o stopOrderResp) (state, actualSide string) {
	switch o.State {
	case stopStateUntriggered:
		return "live", ""
	case stopStateCancelled:
		return "canceled", ""
	case stopStateInvalid, stopStateFailed:
		return "order_failed", ""
	case stopStateExecuted:
		sl := num(o.StopLossPx).IsPositive()
		tp := num(o.TakeProfitPx).IsPositive()
		switch {
		case sl && !tp:
			return "effective", "sl"
		case tp && !sl:
			return "effective", "tp"
		default:
			return "effective", ""
		}
	default:
		return "", ""
	}
}

// positionIDFor finds the open position's id for one symbol and side.
//
// Protection on MEXC is attached to a position, so placing it requires knowing which. A missing
// position is an error rather than a zero id: silently targeting position 0 would either fail
// obscurely at the exchange or, worse, act on an unrelated position.
func (c *Client) positionIDFor(instID, posSide string) (int64, error) {
	var resp []positionResp
	params := map[string]string{"symbol": instID}
	if err := c.do("GET", "/api/v1/private/position/open_positions", params, nil, &resp); err != nil {
		return 0, fmt.Errorf("look up position: %w", err)
	}
	for _, p := range resp {
		if !num(p.HoldVol).IsPositive() {
			continue
		}
		if posSide != "" {
			want := 1
			if strings.EqualFold(posSide, "short") {
				want = 2
			}
			if p.PositionType != want {
				continue
			}
		}
		id, err := p.PositionID.Int64()
		if err != nil {
			return 0, fmt.Errorf("malformed positionId %q: %w", p.PositionID.String(), err)
		}
		return id, nil
	}
	return 0, fmt.Errorf("no open position for %s (side %q)", instID, posSide)
}

// parsePositionID converts the opaque handle back into MEXC's position id.
func parsePositionID(algoID string) (int64, error) {
	if algoID == "" {
		return 0, fmt.Errorf("empty algo id")
	}
	n, err := decimal.NewFromString(algoID)
	if err != nil {
		return 0, fmt.Errorf("malformed algo id %q: %w", algoID, err)
	}
	return n.IntPart(), nil
}
