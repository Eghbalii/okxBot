// Package candlefetch is a standalone historical-candle collection tool — NOT part of any running
// service, deliberately kept out of cmd/ and internal/ (2026-09-22 operator request: "یه ابزار
// شیک... نه داخل دایرکتوری‌های اصلی و سرویس‌ها"). It exists to backfill the `candles` table (the
// same table cmd/paper-trader writes to and internal/backtest reads from, migration 000038's
// (exchange, inst_id, bar, ts) key) for a token list × bar list × historical date range, ahead of
// the operator's planned backtest feature — the backtest tool itself reads from `candles` via
// Repository.ListCandlesRange/CandleRange (internal/postgres/candles.go) and needs no changes to
// consume whatever this tool writes.
//
// Deliberately MEXC-only for now: OKX's only ranged-history endpoint (GET /market/history-candles)
// was removed from this codebase entirely per an explicit, standing operator instruction that it
// must never be called again (CLAUDE.md §33.5) — this tool must not quietly reintroduce it under a
// new name. A future exchange (or a genuinely different OKX endpoint, if one exists) plugs in via
// the same RangeFetcher interface below.
//
// Rate-limit safety is the whole point of this package existing as more than a bare loop of REST
// calls: reuses internal/gateway.Limiter (the same token-bucket admission logic cmd/okx-gateway
// runs in front of real trading, CLAUDE.md §27.1) so a large multi-token/multi-bar job cannot burst
// past the exchange's own rate limit and risk the server's IP being banned, regardless of how many
// goroutines are collecting concurrently.
package candlefetch

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// RangeFetcher is the one exchange capability this tool needs: candles for one instrument/timeframe
// in a bounded time window. Narrow on purpose, the same reasoning as CLAUDE.md §17's
// HistoryCandleFetcher and §53's allTickerFetcher — a backfill tool must not be able to place or
// cancel an order, and the type system is a better guarantee of that than care.
//
// instID here is the EXCHANGE's own wire-format instrument id (Job.ExecInstID, e.g. "BTC_USDT"),
// never this project's short internal symbol — found live during this tool's first real-data test
// (not caught by unit tests, which never touch a real exchange): passing the bare short symbol
// ("BTC") failed every request with MEXC's "Contract does not exist", since that string names
// nothing on the exchange's own side.
//
// A single call is not required to cover the whole [start, end) window — MEXC's own kline endpoint
// has an empirically observed practical cap on how many bars one response carries. Fetch pages by
// re-calling with an advanced start whenever a response looks like it was truncated (returned fewer
// candles than the window should hold, but stopped short of `end`).
type RangeFetcher interface {
	GetCandlesRange(instID, bar string, start, end time.Time) ([]domain.Candle, error)
}

// CandleWriter is the one storage capability this tool needs. port.Repository already satisfies it;
// kept as its own tiny interface so a test can supply an in-memory fake without a real Postgres.
type CandleWriter interface {
	SaveCandle(ctx context.Context, c port.Candle) error
}

// RangeReader is used to derive the "N candles before our earliest existing data" anchor point —
// the operator's own instruction for this first run: don't ask for an arbitrary fixed calendar
// date, walk backward from whatever this database already holds as its oldest candle per
// instrument/bar, since that oldest candle is exactly where continuity should resume from.
type RangeReader interface {
	CandleRange(ctx context.Context, exchange, instID, bar string) (oldest, newest time.Time, err error)
}

// BarDuration reports how long one bar of the given timeframe lasts. Exchange adapters already
// expose this (internal/mexc/rest.BarDuration); passed in rather than imported so this package
// never has to import a specific exchange adapter (the same internal/usecase layering rule
// CLAUDE.md §46.1/TestUsecaseImportsNoExchangeAdapter enforces, applied here on principle even
// though this tool sits outside internal/usecase).
type BarDuration func(bar string) (time.Duration, bool)

// Job describes one instrument/bar's fetch window, resolved ahead of time so Progress can report
// against a known total rather than discovering scope as it goes.
//
// InstID is this project's short internal symbol ("BTC") — what Progress reports and what
// CandleWriter stores candles under (matching every other service, CLAUDE.md §33.4's short-symbol
// design). ExecInstID is the exchange's own wire-format id ("BTC_USDT") — what actually gets sent
// to RangeFetcher. The two are kept as separate fields rather than just using ExecInstID
// everywhere: storing candles under the wire-format id would silently create a second, disconnected
// copy of that token's history under a different key than every other service already writes to.
type Job struct {
	InstID     string
	ExecInstID string
	Bar        string
	Start      time.Time
	End        time.Time
}

// Target is what the caller actually asks for: a token × bar cross product, plus how far back to
// backfill each bar. CandlesBack is per-bar (not one global count) because the operator's own first
// request already needs two different depths in one run: 50 candles back for most bars, 20 for 15m
// specifically — a single global count could not express that.
type Target struct {
	Exchange string
	InstIDs  []string
	Bars     []string
	// ExecInstIDs maps each InstIDs entry to the exchange's own wire-format instrument id — the
	// SAME roster mapping every other service resolves through (usecase.RosterFor's ExecInstID,
	// CLAUDE.md §33.4). A symbol with no entry here is skipped by ResolveJobs (reported, not
	// silently dropped) rather than sent to the exchange as its bare short symbol, which is exactly
	// the "Contract does not exist" failure this field exists to prevent.
	ExecInstIDs map[string]string
	// CandlesBack maps a bar name to how many candles' worth of history to fetch, counted backward
	// from that instrument/bar's own EARLIEST already-stored candle (RangeReader.CandleRange) — not
	// from "now" and not from a fixed calendar date. A bar with no entry here is skipped, so a
	// caller must be explicit about every bar it wants covered.
	CandlesBack map[string]int
}

// Progress is reported after every completed Job, letting a caller render a percentage/ETA without
// this package depending on any particular UI (CLAUDE.md's own repeated "no UI framework" pattern
// extends naturally here — a plain channel of snapshots, render however the caller likes).
type Progress struct {
	Completed   int
	Total       int
	CandlesSaved int64
	Errors      int
	Elapsed     time.Duration
	// ETA is Elapsed scaled by remaining/completed work — zero until at least one job has finished,
	// since there is nothing to extrapolate from before that.
	ETA time.Time
	// Current names the job that just finished, for a log line a human can actually read mid-run.
	Current Job
	LastErr error
}

// Percent is Completed/Total as a whole-number percentage, 0 when Total is 0 (nothing to do is not
// "0% forever", it's just not meaningfully a percentage at all — callers should check Total
// separately before deciding how to render this).
func (p Progress) Percent() float64 {
	if p.Total == 0 {
		return 0
	}
	return float64(p.Completed) / float64(p.Total) * 100
}

// Options tunes the run. Every field has a safe default applied by Fetch when left zero, so a
// caller can construct Options{} and get conservative behavior rather than an unconfigured crash or
// an accidental burst against the exchange.
type Options struct {
	// Concurrency bounds how many (instrument, bar) jobs are fetched at once. Default 4 — modest on
	// purpose: the real ceiling on throughput here is the rate limiter below, not goroutine count,
	// so raising this mainly widens how many jobs are IN FLIGHT waiting on the same limiter rather
	// than actually completing faster; it exists mainly so slow, high-latency requests to a distant
	// exchange don't serialize behind each other for no reason.
	Concurrency int
	// Limits is the token-bucket configuration handed to gateway.Limiter, keyed the same way
	// cmd/okx-gateway's own config is (CLAUDE.md §27.1) — reusing that package's ClassMarket bucket
	// rather than a bespoke limiter for this tool. Defaults to a conservative, hand-picked value
	// (10 req/2s) rather than gateway.DefaultLimits()'s 20/2s: those defaults were sized for a
	// LIVE gateway serving a running trading system that also has the trader's own priority
	// mechanism to fall back on if this tool over-consumes; a standalone offline job has nothing to
	// fall back on and no trader to protect, so it errs tighter. Exposed here specifically so a
	// caller can measure the real exchange's documented limit and tighten/loosen this without
	// touching this package's code.
	Limits *gateway.ClassLimit
	// PageDelay adds a small deliberate pause between successive PAGES of the SAME job (a job whose
	// window needed more than one request because the exchange truncated the response) — on top of,
	// not instead of, the rate limiter. The limiter alone already prevents exceeding the bucket, but
	// a fixed floor between an instrument's own successive requests is what CLAUDE.md §17's original
	// backfill design called "paced, not just rate-limited" — being correct on average is not the
	// same as never bursting, and this trades a small amount of wall-clock time for never being the
	// job that discovers the limiter's numbers were slightly optimistic. Default 200ms.
	PageDelay time.Duration
	// consumer is the gateway.Limiter consumer identity this tool's requests are billed under. Not
	// exported: this tool has exactly one identity and no caller should be choosing a different one
	// per invocation, the same "don't expose a knob nothing should turn" reasoning as internal
	// config defaults elsewhere in this codebase.
	consumer string
}

func (o *Options) applyDefaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.Limits == nil {
		o.Limits = &gateway.ClassLimit{Capacity: 10, Refill: 10, Interval: 2 * time.Second}
	}
	if o.PageDelay <= 0 {
		o.PageDelay = 200 * time.Millisecond
	}
	if o.consumer == "" {
		o.consumer = "candlefetch"
	}
}

// ResolveJobs turns a Target into concrete [start, end) windows, one per (instID, bar), by reading
// each instrument/bar's current earliest stored candle from repo and walking back CandlesBack[bar]
// bars from there. An instrument/bar with NO existing candle at all is skipped (there is no anchor
// to walk back from) and reported in the returned skipped slice rather than silently omitted — the
// same "loud gap over a silent one" rule as CLAUDE.md §9's bar-casing validation.
func ResolveJobs(
	ctx context.Context,
	repo RangeReader,
	barDuration BarDuration,
	target Target,
) (jobs []Job, skipped []string, err error) {
	for _, instID := range target.InstIDs {
		execID, ok := target.ExecInstIDs[instID]
		if !ok || execID == "" {
			skipped = append(skipped, fmt.Sprintf("%s (no exec instID in roster/symbol map)", instID))
			continue
		}
		for _, bar := range target.Bars {
			n, ok := target.CandlesBack[bar]
			if !ok || n <= 0 {
				continue
			}
			dur, ok := barDuration(bar)
			if !ok {
				return nil, nil, fmt.Errorf("candlefetch: unsupported bar %q for exchange %q", bar, target.Exchange)
			}
			oldest, _, err := repo.CandleRange(ctx, target.Exchange, instID, bar)
			if err != nil {
				return nil, nil, fmt.Errorf("candlefetch: read existing range for %s/%s: %w", instID, bar, err)
			}
			if oldest.IsZero() {
				skipped = append(skipped, fmt.Sprintf("%s/%s (no existing candle to anchor from)", instID, bar))
				continue
			}
			jobs = append(jobs, Job{
				InstID:     instID,
				ExecInstID: execID,
				Bar:        bar,
				Start:      oldest.Add(-dur * time.Duration(n)),
				End:        oldest,
			})
		}
	}
	// Deterministic order (instID then bar) so two runs over the same target produce the same
	// progress sequence — makes a log/ETA comparison between runs meaningful, and makes test
	// assertions on job order possible without a separate sort step in every test.
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].InstID != jobs[j].InstID {
			return jobs[i].InstID < jobs[j].InstID
		}
		return jobs[i].Bar < jobs[j].Bar
	})
	return jobs, skipped, nil
}

// Fetch runs every job with bounded concurrency, rate-limited via a shared gateway.Limiter, saving
// every candle returned to writer and sending a Progress snapshot on progressCh after each job
// completes (success or failure — a failed job still advances Completed, since it is no longer
// pending work; LastErr on that snapshot is what distinguishes the two). progressCh is closed when
// every job has finished; the caller is responsible for draining it (a buffered channel of size
// len(jobs) is used internally as the source, so a caller that never reads does not deadlock the
// workers — see the buffering below).
//
// Returns an error only for a setup problem (nothing to do). Per-job failures do not stop the run —
// CLAUDE.md §17's own "partial failure is expected, not fatal" precedent: one bad instrument or one
// transient exchange error must not abandon 150 other tokens' worth of otherwise-good data. Collect
// them from the final Progress snapshot's Errors count, or from progressCh as it streams.
func Fetch(
	ctx context.Context,
	fetcher RangeFetcher,
	writer CandleWriter,
	exchange string,
	jobs []Job,
	opts Options,
) <-chan Progress {
	opts.applyDefaults()

	progressCh := make(chan Progress, len(jobs)+1)
	if len(jobs) == 0 {
		close(progressCh)
		return progressCh
	}

	limiter := gateway.NewLimiter(map[gateway.EndpointClass]gateway.ClassLimit{
		gateway.ClassMarket: *opts.Limits,
	})

	var (
		completed int64
		saved     int64
		errCount  int64
	)
	start := time.Now()

	jobCh := make(chan Job, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	var wg sync.WaitGroup
	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				n, err := fetchAndSaveOne(ctx, fetcher, writer, limiter, exchange, job, opts)
				atomic.AddInt64(&saved, int64(n))
				if err != nil {
					atomic.AddInt64(&errCount, 1)
				}
				done := atomic.AddInt64(&completed, 1)
				elapsed := time.Since(start)
				p := Progress{
					Completed:    int(done),
					Total:        len(jobs),
					CandlesSaved: atomic.LoadInt64(&saved),
					Errors:       int(atomic.LoadInt64(&errCount)),
					Elapsed:      elapsed,
					Current:      job,
					LastErr:      err,
				}
				if done > 0 {
					perJob := elapsed / time.Duration(done)
					remaining := time.Duration(len(jobs)-int(done)) * perJob
					p.ETA = time.Now().Add(remaining)
				}
				progressCh <- p
			}
		}()
	}

	go func() {
		wg.Wait()
		close(progressCh)
	}()

	return progressCh
}

// fetchAndSaveOne pages through one job's [Start, End) window, saving every candle as it arrives
// (rather than buffering the whole job in memory first) so a long-running multi-page job still
// makes visible progress in the candles table if the process is interrupted partway.
func fetchAndSaveOne(
	ctx context.Context,
	fetcher RangeFetcher,
	writer CandleWriter,
	limiter *gateway.Limiter,
	exchange string,
	job Job,
	opts Options,
) (saved int, err error) {
	cursor := job.Start
	first := true
	for cursor.Before(job.End) {
		if !first {
			select {
			case <-ctx.Done():
				return saved, ctx.Err()
			case <-time.After(opts.PageDelay):
			}
		}
		first = false

		if err := limiter.Acquire(ctx, gateway.ClassMarket, opts.consumer, gateway.PriorityNormal); err != nil {
			return saved, fmt.Errorf("candlefetch: rate limiter: %w", err)
		}

		candles, err := fetcher.GetCandlesRange(job.ExecInstID, job.Bar, cursor, job.End)
		if err != nil {
			return saved, fmt.Errorf("candlefetch: fetch %s/%s [%s, %s): %w",
				job.InstID, job.Bar, cursor.Format(time.RFC3339), job.End.Format(time.RFC3339), err)
		}
		if len(candles) == 0 {
			// Nothing left in this window — either it's exhausted or the exchange has no data this
			// far back for this instrument (a young listing). Either way, stop; this is not an error.
			break
		}

		newest := candles[0].Timestamp
		for _, c := range candles {
			if err := writer.SaveCandle(ctx, port.Candle{
				InstID: job.InstID, Bar: job.Bar, Exchange: exchange, Candle: c,
			}); err != nil {
				return saved, fmt.Errorf("candlefetch: save %s/%s@%s: %w",
					job.InstID, job.Bar, c.Timestamp.Format(time.RFC3339), err)
			}
			saved++
			if c.Timestamp.After(newest) {
				newest = c.Timestamp
			}
		}

		if !newest.After(cursor) {
			// The exchange returned data but none of it advanced the window — a malformed/looping
			// response would otherwise spin this loop forever re-requesting the same range.
			break
		}
		cursor = newest.Add(time.Nanosecond) // strictly past the last saved candle, never re-fetch it
	}
	return saved, nil
}
