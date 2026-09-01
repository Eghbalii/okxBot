package gatewayclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// Compile-time check: gatewayclient.Client must implement port.ExchangeClient, since that's the
// whole point — cmd/trader swaps rest.Client for this with no other code change (CLAUDE.md
// §27.1).
var _ port.ExchangeClient = (*Client)(nil)

func TestGetTicker_SendsConsumerHeaderAndDecodesResponse(t *testing.T) {
	var gotConsumer string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConsumer = r.Header.Get("X-Gateway-Consumer")
		gotPath = r.URL.String()
		json.NewEncoder(w).Encode(domain.Ticker{InstID: "BTC-USDT-SWAP", Last: decimal.NewFromInt(50000)})
	}))
	defer srv.Close()

	c := New(srv.URL, "trader")
	ticker, err := c.GetTicker("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if ticker.InstID != "BTC-USDT-SWAP" || !ticker.Last.Equal(decimal.NewFromInt(50000)) {
		t.Fatalf("unexpected ticker: %+v", ticker)
	}
	if gotConsumer != "trader" {
		t.Fatalf("expected X-Gateway-Consumer=trader, got %q", gotConsumer)
	}
	if gotPath != "/ticker?instId=BTC-USDT-SWAP" {
		t.Fatalf("unexpected request path: %q", gotPath)
	}
}

func TestPlaceOrder_SendsBodyAndDecodesResult(t *testing.T) {
	var gotReq domain.OrderRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/order" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(domain.OrderResult{OrdID: "999", SCode: "0"})
	}))
	defer srv.Close()

	c := New(srv.URL, "trader")
	req := domain.OrderRequest{InstID: "BTC-USDT-SWAP", TdMode: "isolated", Side: "buy", OrdType: "market", Sz: decimal.NewFromInt(1)}
	result, err := c.PlaceOrder(req)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if result.OrdID != "999" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if gotReq.InstID != "BTC-USDT-SWAP" || gotReq.TdMode != "isolated" {
		t.Fatalf("gateway did not receive the expected request body: %+v", gotReq)
	}
}

func TestDo_SurfacesGatewayErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "okx api error: code=51008 msg=insufficient balance"})
	}))
	defer srv.Close()

	c := New(srv.URL, "trader")
	_, err := c.PlaceOrder(domain.OrderRequest{InstID: "BTC-USDT-SWAP"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected a non-empty error message")
	}
}

func TestGetOrder_BuildsQueryParams(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		json.NewEncoder(w).Encode(domain.OrderStatus{InstID: "BTC-USDT-SWAP", OrdID: "42", State: "filled"})
	}))
	defer srv.Close()

	c := New(srv.URL, "trader")
	status, err := c.GetOrder("BTC-USDT-SWAP", "42")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if status.State != "filled" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if gotPath != "/order?instId=BTC-USDT-SWAP&ordId=42" {
		t.Fatalf("unexpected request path: %q", gotPath)
	}
}

func TestCancelOrder_SendsInstIDAndOrdID(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer srv.Close()

	c := New(srv.URL, "paper-trader")
	if err := c.CancelOrder("BTC-USDT-SWAP", "42"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if gotBody["instId"] != "BTC-USDT-SWAP" || gotBody["ordId"] != "42" {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
}

func TestSetLeverage_SendsRequestBody(t *testing.T) {
	var gotReq domain.LeverageChange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "trader")
	req := domain.LeverageChange{InstID: "BTC-USDT-SWAP", Lever: decimal.NewFromInt(10), MgnMode: "isolated"}
	if err := c.SetLeverage(req); err != nil {
		t.Fatalf("SetLeverage: %v", err)
	}
	if gotReq.InstID != "BTC-USDT-SWAP" || gotReq.MgnMode != "isolated" {
		t.Fatalf("unexpected request body: %+v", gotReq)
	}
}
