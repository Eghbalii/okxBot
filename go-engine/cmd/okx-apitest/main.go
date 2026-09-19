// Command okx-apitest is a standalone, throwaway diagnostic for verifying real OKX API
// credentials work end-to-end (CLAUDE.md's 2026-09-04 API-key note) — it is NOT part of the
// trading pipeline, connects to no Kafka topic, evaluates no strategy, and opens no position on
// its own initiative. Every order it places is explicit, hardcoded in this file, and priced to
// never fill (see farFromMarketPrice below) so the test can run against the real account without
// risking an actual fill.
//
// SAFETY: this binary must only ever be run on the server, with real credentials supplied via
// environment variables at invocation time (OKX_API_KEY / OKX_API_SECRET / OKX_API_PASSPHRASE).
// Never put real credentials in a file inside this repo. See CLAUDE.md's API-key note.
//
// Usage: OKX_API_KEY=... OKX_API_SECRET=... OKX_API_PASSPHRASE=... go run ./cmd/okx-apitest
//
// What it does, in order (matches the operator's own test plan exactly):
//  1. GET account balance, GET open positions — confirms the key can read account state.
//  2. Places a BTC-USDT-SWAP LIMIT sell order priced +5% above the current market price (a sell
//     limit above market never fills against a market that hasn't rallied 5% in the test's
//     lifetime), with attached TP/SL fields on the SAME order request (OKX's "Way 1": tpTriggerPx/
//     tpOrdPx/slTriggerPx/slOrdPx) to verify that attach-on-open path works.
//  3. Waits up to 60s using BotTrader.WaitForFillForTesting — a thin export of the actual
//     production timeout/cancel function (CLAUDE.md §27.5), not a reimplementation — to confirm
//     the real timeout+cancel logic actually cancels an order that will never fill. If it does NOT
//     come back canceled, this script cancels it manually and prints a clearly flagged BUG line.
//  4. Places a second such order (no attached TP/SL). Fetches open positions (expected: none,
//     since a limit order that hasn't filled is not a position). Attaches SL via a separate
//     order-algo call (OKX's "Way 2"), then amends that algo order to a new trigger price,
//     fetching positions again to confirm state, then cancels both the algo order and the parent
//     order.
//
// Every step logs the raw OKX response so a human can verify each call actually did what it
// claims, rather than trusting only this script's own pass/fail summary.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/okx/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// instID/tdMode/sz are overridable via OKX_TEST_INST/OKX_TEST_TDMODE/OKX_TEST_SZ so this
// diagnostic can be re-pointed at a different instrument/margin-mode/size while iterating on why
// a given market rejects an order, without a rebuild each time. Defaults match §27.2/§26's
// configured real-trading posture.
var (
	instID = envOr("OKX_TEST_INST", "BTC-USD-SWAP")
	tdMode = envOr("OKX_TEST_TDMODE", "isolated")
	sz     = envOr("OKX_TEST_SZ", "0.1")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	apiKey := os.Getenv("OKX_API_KEY")
	apiSecret := os.Getenv("OKX_API_SECRET")
	passphrase := os.Getenv("OKX_API_PASSPHRASE")
	if apiKey == "" || apiSecret == "" || passphrase == "" {
		log.Fatal("OKX_API_KEY, OKX_API_SECRET, OKX_API_PASSPHRASE must all be set in the environment")
	}

	simulated := os.Getenv("OKX_SIMULATED") == "1"
	baseURL := os.Getenv("OKX_BASE_URL")
	if baseURL == "" {
		// EEA-hosted servers (this project's server is OVH/Roubaix, France) must use my.okx.com —
		// www.okx.com rejects EEA-originated requests with a misleading "50119 API key doesn't
		// exist" error even for a fully valid key/IP-whitelist combination (found 2026-09-04).
		baseURL = "https://my.okx.com"
	}

	client := rest.New(baseURL, apiKey, apiSecret, passphrase, simulated)
	raw := rawClient{apiKey: apiKey, apiSecret: apiSecret, passphrase: passphrase, simulated: simulated, baseURL: baseURL}
	algo := &algoClient{client: raw}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()

	fmt.Println("=== Step 0: account balance + positions + config ===")
	mustPrintBalance(client)
	mustPrintFundingBalance(raw)
	mustPrintRiskState(raw)
	mustPrintPositions(client)
	mustPrintAccountConfig(raw)
	mustPrintLeverageInfo(raw)
	mustPrintMaxSize(raw)

	fmt.Println()
	fmt.Println("=== Step 1: place order #1 (attached TP/SL), verify 60s timeout+cancel ===")
	runOrder1(ctx, client, raw, logger)

	fmt.Println()
	fmt.Println("=== Step 2: place order #2, attach SL via order-algo, amend, verify, cancel ===")
	runOrder2(client, raw, algo)

	fmt.Println()
	fmt.Println("=== Done. Review the log above for any line starting with BUG: ===")
}

// farFromMarketPrice returns a sell-limit price 5% above the current last-traded price, high
// enough that it will not fill during this test's short lifetime.
func farFromMarketPrice(client *rest.Client) decimal.Decimal {
	ticker, err := client.GetTicker(instID)
	if err != nil {
		log.Fatalf("GetTicker failed: %v", err)
	}
	if ticker.Last.IsZero() {
		log.Fatalf("GetTicker returned zero last price — cannot compute a safe test price")
	}
	px := ticker.Last.Mul(decimal.NewFromFloat(1.05)).Round(1)
	fmt.Printf("  current last price: %s, test limit price (+5%%): %s\n", ticker.Last.String(), px.String())
	return px
}

func mustPrintBalance(client *rest.Client) {
	balances, err := client.GetBalance("")
	if err != nil {
		log.Fatalf("GetBalance failed: %v", err)
	}
	b, _ := json.MarshalIndent(balances, "  ", "  ")
	fmt.Printf("  balances: %s\n", b)
}

func mustPrintFundingBalance(raw rawClient) {
	data, rawResp, err := raw.do("GET", "/api/v5/asset/balances", nil)
	if err != nil {
		fmt.Printf("  GetFundingBalance failed: %v (raw=%s)\n", err, rawResp)
		return
	}
	fmt.Printf("  funding account balances: %s\n", string(data))
}

func mustPrintRiskState(raw rawClient) {
	data, rawResp, err := raw.do("GET", "/api/v5/account/risk-state", nil)
	if err != nil {
		fmt.Printf("  GetRiskState failed: %v (raw=%s)\n", err, rawResp)
		return
	}
	fmt.Printf("  risk state: %s\n", string(data))
}

func mustPrintAccountConfig(raw rawClient) {
	data, rawResp, err := raw.do("GET", "/api/v5/account/config", nil)
	if err != nil {
		fmt.Printf("  GetAccountConfig failed: %v (raw=%s)\n", err, rawResp)
		return
	}
	fmt.Printf("  account config: %s\n", string(data))
}

func mustPrintLeverageInfo(raw rawClient) {
	path := "/api/v5/account/leverage-info?instId=" + instID + "&mgnMode=" + tdMode
	data, rawResp, err := raw.do("GET", path, nil)
	if err != nil {
		fmt.Printf("  GetLeverageInfo failed: %v (raw=%s)\n", err, rawResp)
		return
	}
	fmt.Printf("  leverage info: %s\n", string(data))
}

func mustPrintMaxSize(raw rawClient) {
	path := "/api/v5/account/max-size?instId=" + instID + "&tdMode=" + tdMode
	data, rawResp, err := raw.do("GET", path, nil)
	if err != nil {
		fmt.Printf("  GetMaxSize failed: %v (raw=%s)\n", err, rawResp)
		return
	}
	fmt.Printf("  max size: %s\n", string(data))
}

func mustPrintPositions(client *rest.Client) {
	positions, err := client.GetPositions("SWAP")
	if err != nil {
		log.Fatalf("GetPositions failed: %v", err)
	}
	b, _ := json.MarshalIndent(positions, "  ", "  ")
	fmt.Printf("  open SWAP positions: %s\n", b)
}

func runOrder1(ctx context.Context, client *rest.Client, raw rawClient, logger *slog.Logger) {
	px := farFromMarketPrice(client)
	// This is a SELL (opening a short): TP must be BELOW the entry price (profit as price falls),
	// SL must be ABOVE it (loss as price rises) — OKX rejects the inverted combination (51048).
	tpTrigger := px.Mul(decimal.NewFromFloat(0.90)).Round(1) // below entry
	slTrigger := px.Mul(decimal.NewFromFloat(1.10)).Round(1) // above entry — never realistically hit

	result, rawResp, err := raw.placeAttachedOrder(instID, tdMode, "sell", "short", "limit", sz, px.String(), tpTrigger.String(), slTrigger.String())
	if err != nil {
		log.Fatalf("place order #1 failed: %v (raw=%s)", err, rawResp)
	}
	fmt.Printf("  placed order #1: ordId=%s sCode=%s sMsg=%s raw=%s\n", result.OrdID, result.SCode, result.SMsg, rawResp)
	if result.SCode != "0" {
		log.Fatalf("order #1 was rejected by OKX (sCode=%s sMsg=%s) — stopping", result.SCode, result.SMsg)
	}

	trader := &usecase.BotTrader{
		InstID:      instID,
		Exchange:    client,
		FillTimeout: 60 * time.Second,
	}

	fmt.Println("  waiting up to 60s via BotTrader.WaitForFillForTesting (the real production timeout+cancel path)...")
	status, err := trader.WaitForFillForTesting(ctx, result.OrdID, logger)
	if err != nil {
		fmt.Printf("  WaitForFillForTesting returned error: %v\n", err)
	}
	fmt.Printf("  order #1 final observed state: %+v\n", status)

	// Confirm cancellation actually took by asking OKX directly, independent of the wait's own
	// return value.
	final, err := client.GetOrder(instID, result.OrdID)
	if err != nil {
		fmt.Printf("  GetOrder after wait failed: %v\n", err)
		return
	}
	fmt.Printf("  order #1 GetOrder after wait: state=%s\n", final.State)
	if final.State != "canceled" {
		fmt.Printf("  BUG: order #1 was NOT canceled by the timeout logic (state=%s). Canceling manually now.\n", final.State)
		if err := client.CancelOrder(instID, result.OrdID); err != nil {
			fmt.Printf("  BUG: manual cancel also failed: %v\n", err)
		} else {
			fmt.Println("  manual cancel succeeded. Investigate BotTrader.waitForFill / fillTimeout before relying on this in production.")
		}
		return
	}
	fmt.Println("  OK: order #1 was correctly auto-canceled by the timeout logic.")
}

func runOrder2(client *rest.Client, raw rawClient, algo *algoClient) {
	px := farFromMarketPrice(client)

	result, rawResp, err := raw.placePlainOrder(instID, tdMode, "sell", "short", "limit", sz, px.String())
	if err != nil {
		log.Fatalf("place order #2 failed: %v (raw=%s)", err, rawResp)
	}
	fmt.Printf("  placed order #2: ordId=%s sCode=%s sMsg=%s raw=%s\n", result.OrdID, result.SCode, result.SMsg, rawResp)
	if result.SCode != "0" {
		log.Fatalf("order #2 was rejected by OKX (sCode=%s sMsg=%s) — stopping", result.SCode, result.SMsg)
	}

	fmt.Println("  fetching open positions (expect none — order #2 has not filled):")
	mustPrintPositions(client)

	// Short position: SL must be ABOVE entry (loss as price rises).
	slTrigger := px.Mul(decimal.NewFromFloat(1.10)).Round(1)
	algoID, algoRaw, err := algo.placeSLTP(instID, tdMode, "sell", "short", sz, slTrigger.String())
	if err != nil {
		log.Fatalf("order-algo place failed: %v (raw=%s)", err, algoRaw)
	}
	fmt.Printf("  attached SL via order-algo: algoId=%s raw=%s\n", algoID, algoRaw)

	newSLTrigger := px.Mul(decimal.NewFromFloat(1.08)).Round(1)
	amendRaw, err := algo.amendSL(instID, algoID, newSLTrigger.String())
	if err != nil {
		log.Fatalf("order-algo amend failed: %v (raw=%s)", err, amendRaw)
	}
	fmt.Printf("  amended SL trigger to %s: raw=%s\n", newSLTrigger.String(), amendRaw)

	fmt.Println("  fetching open positions again to confirm state after amend:")
	mustPrintPositions(client)

	fmt.Println("  canceling algo order:")
	cancelAlgoRaw, err := algo.cancel(instID, algoID)
	if err != nil {
		fmt.Printf("  BUG: order-algo cancel failed: %v (raw=%s)\n", err, cancelAlgoRaw)
	} else {
		fmt.Printf("  algo order canceled: raw=%s\n", cancelAlgoRaw)
	}

	fmt.Println("  canceling parent order #2:")
	if err := client.CancelOrder(instID, result.OrdID); err != nil {
		fmt.Printf("  BUG: parent order #2 cancel failed: %v\n", err)
	} else {
		fmt.Println("  order #2 canceled.")
	}

	final, err := client.GetOrder(instID, result.OrdID)
	if err == nil {
		fmt.Printf("  order #2 final state: %s\n", final.State)
	}
}
