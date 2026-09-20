package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetAccountConfig_ReadsPosMode confirms the GET /api/v5/account/config response is decoded
// into domain.AccountConfig.PosMode correctly — OKX's own two wire values, unchanged (this type
// exists specifically to round-trip against the exchange, domain.AccountConfig's own doc comment).
func TestGetAccountConfig_ReadsPosMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/account/config" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":"0","data":[{"posMode":"long_short_mode"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "s", "p", false)

	cfg, err := c.GetAccountConfig()
	if err != nil {
		t.Fatalf("GetAccountConfig: %v", err)
	}
	if cfg.PosMode != "long_short_mode" {
		t.Errorf("PosMode = %q, want long_short_mode", cfg.PosMode)
	}
}

// TestGetAccountConfig_NoDataIsAnError guards against silently returning a zero-value AccountConfig
// (which would read as "net mode" — the more restrictive default) when OKX's response is genuinely
// empty, rather than surfacing that as a real error the caller can act on.
func TestGetAccountConfig_NoDataIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","data":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "s", "p", false)

	if _, err := c.GetAccountConfig(); err == nil {
		t.Fatal("expected an error for an empty data array, got nil")
	}
}

// TestSetPositionMode_SendsPosModeOnTheWire drives the real client against a stub server and
// asserts the exact body sent, the same "assert what the code actually sends" discipline
// TestAlgoOrders_SendTriggerPriceType already established in this package.
func TestSetPositionMode_SendsPosModeOnTheWire(t *testing.T) {
	var sent map[string]string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = w.Write([]byte(`{"code":"0","data":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "s", "p", false)

	if err := c.SetPositionMode("long_short_mode"); err != nil {
		t.Fatalf("SetPositionMode: %v", err)
	}
	if path != "/api/v5/account/set-position-mode" {
		t.Errorf("path = %q", path)
	}
	if sent["posMode"] != "long_short_mode" {
		t.Errorf("posMode = %q, want long_short_mode", sent["posMode"])
	}
}

// TestSetPositionMode_SurfacesTheExchangesOwnRejection confirms OKX's rejection (which fires when
// the account has open positions/pending orders, per its own docs) reaches the caller as a real
// error rather than being swallowed — the caller's own pre-check is a friendlier first line of
// defense, but the exchange's own answer must still be trusted as authoritative either way
// (CLAUDE.md §27.6/§49.2's "ask the exchange, don't just trust local state" precedent).
func TestSetPositionMode_SurfacesTheExchangesOwnRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"1","data":[{"sCode":"59000","sMsg":"Please cancel all open orders before switching position mode"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "s", "p", false)

	err := c.SetPositionMode("long_short_mode")
	if err == nil {
		t.Fatal("expected the exchange's rejection to surface as an error")
	}
}
