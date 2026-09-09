package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// OKX rejects a take-profit side that arrives without its trigger-price TYPE:
//
//	{"code":"1","data":[{"sCode":"51000","sMsg":"Parameter newTpTriggerPxType error"}]}
//
// That failure (2026-09-10) made every manual SL/TP edit fail, and on the place path it silently
// cost every position its take-profit — positions opened with a stop and no target at all.
//
// Driven through the REAL client against a stub server, so it asserts what the code actually
// sends rather than a copy of it.
func TestAlgoOrders_SendTriggerPriceType(t *testing.T) {
	var placed, amended map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(r.URL.Path, "amend-algos") {
			amended = body
		} else {
			placed = body
		}
		_, _ = w.Write([]byte(`{"code":"0","data":[{"algoId":"a1","sCode":"0","sMsg":""}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "s", "p", false)

	if _, err := c.PlaceAlgoOrder(domain.AlgoOrderRequest{
		InstID: "ETH", TdMode: "cross", Side: "sell", Sz: decimal.NewFromInt(1),
		SLTriggerPx: decimal.RequireFromString("100"), TPTriggerPx: decimal.RequireFromString("110"),
	}); err != nil {
		t.Fatalf("place: %v", err)
	}
	if placed["slTriggerPxType"] != "last" || placed["tpTriggerPxType"] != "last" {
		t.Errorf("place must send a trigger type for both sides, got %v", placed)
	}

	if err := c.AmendAlgoOrder(domain.AlgoOrderAmend{
		InstID: "ETH", AlgoID: "a1",
		SLTriggerPx: decimal.RequireFromString("100"), TPTriggerPx: decimal.RequireFromString("110"),
	}); err != nil {
		t.Fatalf("amend: %v", err)
	}
	if amended["newSlTriggerPxType"] != "last" || amended["newTpTriggerPxType"] != "last" {
		t.Errorf("amend must send a trigger type for both sides, got %v", amended)
	}
}

// An amend naming neither side is refused locally rather than sent — the guard counted body keys
// before, which the added type fields would have quietly broken.
func TestAmendAlgoOrder_RefusesWithNoLevels(t *testing.T) {
	c := New("http://unused", "k", "s", "p", false)
	if err := c.AmendAlgoOrder(domain.AlgoOrderAmend{InstID: "ETH", AlgoID: "a1"}); err == nil {
		t.Fatal("an amend with no levels must be refused before it reaches the exchange")
	}
}

// A code=1 response carries an EMPTY top-level msg — the real reason is per-item in data[].sMsg.
// Reporting only the envelope produced a bare "code=1 msg=" that named nothing.
func TestFirstItemError_ExtractsThePerItemReason(t *testing.T) {
	data := json.RawMessage(`[{"algoId":"1","sCode":"51000","sMsg":"Parameter newTpTriggerPxType error"}]`)
	got := firstItemError(data)
	if !strings.Contains(got, "51000") || !strings.Contains(got, "newTpTriggerPxType") {
		t.Fatalf("want the per-item sCode and sMsg, got %q", got)
	}
}

// A successful item, or a payload with no per-item detail at all, must fall back to the envelope
// rather than inventing an error.
func TestFirstItemError_EmptyWhenNothingFailed(t *testing.T) {
	for _, raw := range []string{
		`[{"algoId":"1","sCode":"0","sMsg":""}]`,
		`[]`,
		`{"not":"an array"}`,
	} {
		if got := firstItemError(json.RawMessage(raw)); got != "" {
			t.Errorf("%s: want no detail, got %q", raw, got)
		}
	}
}
