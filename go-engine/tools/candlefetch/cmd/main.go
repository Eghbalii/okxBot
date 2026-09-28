// Command candlefetch is the CLI entry point for tools/candlefetch — a standalone historical-candle
// collection job, run manually from the shell, never as a Docker Compose service (2026-09-22
// operator request: a tool "outside the main directories/services", separate from every long-lived
// process this project otherwise runs). It connects directly to MEXC and Postgres; it does not go
// through mexc-gateway, since it is not part of the live trading pipeline the gateway exists to
// protect the rate budget of (CLAUDE.md §27.1) — this tool has its own independent, conservative
// rate limiter instead (tools/candlefetch.Options.Limits).
//
// Example (this session's actual first run — every currently-tradeable MEXC token, 50 candles back
// on every bar except 15m, 20 back on 15m):
//
//	go run ./tools/candlefetch/cmd \
//	  -postgres-dsn "postgres://okxbot:okxbot@localhost:5432/okxbot" \
//	  -mexc-base-url "https://contract.mexc.com" \
//	  -exchange mexc \
//	  -bars 5m,15m,1H,4H,1D \
//	  -candles-back 50 \
//	  -candles-back-15m 20 \
//	  -tokens-from-roster
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
	mexcrest "github.com/eghbalii/okxBot/go-engine/internal/mexc/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/tools/candlefetch"
)

func main() {
	var (
		postgresDSN    = flag.String("postgres-dsn", envOr("POSTGRES_DSN", ""), "Postgres DSN (or POSTGRES_DSN env)")
		mexcBaseURL    = flag.String("mexc-base-url", envOr("MEXC_BASE_URL", "https://contract.mexc.com"), "MEXC REST base URL")
		exchange       = flag.String("exchange", "mexc", "exchange name to store candles under (candles.exchange)")
		tokensFlag     = flag.String("tokens", "", "comma-separated short symbols (e.g. BTC,ETH). Ignored if -tokens-from-roster is set")
		tokensFromDB   = flag.Bool("tokens-from-roster", false, "load the token list from the instruments table (exchange-scoped) instead of -tokens")
		barsFlag       = flag.String("bars", "5m,15m,1H,4H,1D", "comma-separated bars to backfill")
		candlesBack    = flag.Int("candles-back", 50, "default candles to fetch, counted backward from each bar's own earliest stored candle")
		perBarOverride = flag.String("candles-back-overrides", "15m=20", "comma-separated bar=count overrides on top of -candles-back (e.g. \"15m=20,1D=10\")")
		concurrency    = flag.Int("concurrency", 4, "number of (instrument, bar) jobs fetched concurrently")
		rateCapacity   = flag.Int("rate-capacity", 10, "rate limiter token bucket capacity")
		rateInterval   = flag.Duration("rate-interval", 2*time.Second, "rate limiter refill interval")
		pageDelay      = flag.Duration("page-delay", 200*time.Millisecond, "extra pause between successive pages of the same job")
		dryRun         = flag.Bool("dry-run", false, "resolve and print the job list/window sizes without fetching or writing anything")
	)
	flag.Parse()

	if *postgresDSN == "" {
		log.Fatal("candlefetch: -postgres-dsn (or POSTGRES_DSN) is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := postgres.New(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("candlefetch: connect postgres: %v", err)
	}
	defer repo.Close()

	tokens, execIDs, err := resolveTokens(ctx, repo, *exchange, *tokensFlag, *tokensFromDB)
	if err != nil {
		log.Fatalf("candlefetch: resolve tokens: %v", err)
	}
	if len(tokens) == 0 {
		log.Fatal("candlefetch: no tokens to fetch (pass -tokens or -tokens-from-roster)")
	}

	bars := splitNonEmpty(*barsFlag)
	if len(bars) == 0 {
		log.Fatal("candlefetch: -bars must name at least one bar")
	}

	back, err := candlesBackFor(bars, *candlesBack, *perBarOverride)
	if err != nil {
		log.Fatalf("candlefetch: %v", err)
	}

	target := candlefetch.Target{
		Exchange:    *exchange,
		InstIDs:     tokens,
		ExecInstIDs: execIDs,
		Bars:        bars,
		CandlesBack: back,
	}

	jobs, skipped, err := candlefetch.ResolveJobs(ctx, repo, mexcrest.BarDuration, target)
	if err != nil {
		log.Fatalf("candlefetch: resolve jobs: %v", err)
	}
	for _, s := range skipped {
		log.Printf("candlefetch: skipping %s — no existing candle to anchor backward from (run the live ingestor first)", s)
	}
	log.Printf("candlefetch: %d tokens x %d bars = %d job(s) to run (%d skipped)", len(tokens), len(bars), len(jobs), len(skipped))

	if *dryRun {
		for _, j := range jobs {
			log.Printf("  %-14s %-5s  %s  ->  %s", j.InstID, j.Bar, j.Start.Format(time.RFC3339), j.End.Format(time.RFC3339))
		}
		return
	}
	if len(jobs) == 0 {
		log.Println("candlefetch: nothing to do")
		return
	}

	client := mexcrest.New(*mexcBaseURL, "", "") // public candle endpoint needs no credentials

	limit := gateway.ClassLimit{Capacity: *rateCapacity, Refill: *rateCapacity, Interval: *rateInterval}
	progressCh := candlefetch.Fetch(ctx, client, repo, *exchange, jobs, candlefetch.Options{
		Concurrency: *concurrency,
		Limits:      &limit,
		PageDelay:   *pageDelay,
	})

	var last candlefetch.Progress
	for p := range progressCh {
		last = p
		bar := progressBar(p.Percent())
		eta := "-"
		if !p.ETA.IsZero() {
			eta = p.ETA.Format("15:04:05")
		}
		status := "ok"
		if p.LastErr != nil {
			status = "ERROR: " + p.LastErr.Error()
		}
		log.Printf("[%s] %5.1f%% (%d/%d)  candles=%d errors=%d  eta=%s  %s/%s %s",
			bar, p.Percent(), p.Completed, p.Total, p.CandlesSaved, p.Errors, eta, p.Current.InstID, p.Current.Bar, status)
	}

	log.Printf("candlefetch: done. %d job(s), %d candle(s) saved, %d error(s), elapsed=%s",
		last.Total, last.CandlesSaved, last.Errors, last.Elapsed.Round(time.Second))
	if last.Errors > 0 {
		os.Exit(1)
	}
}

// resolveTokens returns the token list to fetch, plus each one's exchange wire-format instrument id
// (Job.ExecInstID) — required by every real RangeFetcher call, found the hard way during this
// tool's first live test: sending the bare short symbol ("BTC") to MEXC fails every request with
// "Contract does not exist", since that string names nothing on the exchange's own side.
//
// The instruments table (the same roster the discovery scan writes and every live service reads,
// internal/usecase/roster.go) is the ONE source for this mapping regardless of whether the caller
// asked for -tokens-from-roster or an explicit -tokens list — an explicit list still needs its
// symbols resolved to real exec ids, and the roster is the only place that mapping lives.
func resolveTokens(ctx context.Context, repo *postgres.Repository, exchange, tokensFlag string, fromRoster bool) (tokens []string, execIDs map[string]string, err error) {
	instruments, err := repo.ListInstruments(ctx, port.InstrumentFilter{Exchange: exchange})
	if err != nil {
		return nil, nil, fmt.Errorf("list instruments: %w", err)
	}
	execIDs = make(map[string]string, len(instruments))
	for _, in := range instruments {
		if in.ExecInstID != "" {
			execIDs[in.Symbol] = in.ExecInstID
		}
	}

	if fromRoster {
		for _, in := range instruments {
			if in.EnabledIngest {
				tokens = append(tokens, in.Symbol)
			}
		}
		sort.Strings(tokens)
		return tokens, execIDs, nil
	}
	return splitNonEmpty(tokensFlag), execIDs, nil
}

// candlesBackFor builds the per-bar CandlesBack map: default applies to every bar, then
// perBarOverride's "bar=count" pairs override individual entries — this is what lets one invocation
// express the operator's own first request (50 back everywhere, 20 back specifically on 15m)
// without a separate flag per bar.
func candlesBackFor(bars []string, def int, overrideSpec string) (map[string]int, error) {
	out := make(map[string]int, len(bars))
	for _, b := range bars {
		out[b] = def
	}
	for _, pair := range splitNonEmpty(overrideSpec) {
		bar, countStr, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("invalid -candles-back-overrides entry %q (want bar=count)", pair)
		}
		bar, countStr = strings.TrimSpace(bar), strings.TrimSpace(countStr)
		count, err := strconv.Atoi(countStr)
		if err != nil {
			return nil, fmt.Errorf("invalid -candles-back-overrides count in %q: %w", pair, err)
		}
		out[bar] = count
	}
	return out, nil
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// progressBar renders a fixed-width ASCII bar — the "percentage and timing" reporting the operator
// asked for, kept as plain terminal text since this tool has no other UI and never will (it's a
// one-shot CLI job, not a service with a panel).
func progressBar(pct float64) string {
	const width = 24
	filled := int(pct / 100 * width)
	if filled > width {
		filled = width
	}
	return strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
}
