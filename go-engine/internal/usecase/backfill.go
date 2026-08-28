package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// Backfill loads historical candles from the exchange into Postgres, so warm-start training
// (CLAUDE.md §15.8) has real market history to roll out against without waiting days for the live
// ingestor to accumulate it.
//
// Sized by CANDLE COUNT per timeframe, not by elapsed days. ReplayEnv advances one candle per step,
// so candle count is what training actually consumes: "30 days" is ~30 rows on a 1D bar and ~8,600
// on 5m. Asking for a fixed number of days would leave the higher timeframes with almost nothing
// while over-fetching the lower ones.
type Backfill struct {
	Exchange port.HistoryCandleFetcher
	Repo     port.Repository
	Logger   *slog.Logger

	// PageDelay throttles between pages. OKX's market endpoints are rate-limited per IP, and a
	// backfill is the one operation that will hit that limit — it issues hundreds of sequential
	// requests where the rest of the system issues one at startup.
	PageDelay time.Duration
}

// DefaultPageDelay paces requests well inside OKX's published limit for /market/history-candles
// (20 requests / 2s per IP). A backfill is not latency-sensitive and shares the IP with the live
// trading path, so it deliberately leaves headroom rather than racing to the ceiling — being
// rate-limited here would also throttle order placement.
const DefaultPageDelay = 150 * time.Millisecond

// DefaultTargetCandles is how many candles per (instrument, timeframe) a backfill aims for when no
// explicit target is configured. 1,500 gives every timeframe a window long enough to be worth
// training on: ~5 days of 5m, ~15 days of 15m, ~2 months of 1H, ~4 years of 1D.
const DefaultTargetCandles = 1500

// BackfillRequest describes one backfill run.
type BackfillRequest struct {
	InstIDs []string
	Bars    []string
	// TargetCandles per (instrument, bar). Zero means DefaultTargetCandles.
	TargetCandles int
}

// BackfillResult reports what one (instrument, bar) pair actually produced. Errors are reported
// per pair rather than aborting the run: one instrument being delisted, or one timeframe having
// less history than requested, should not discard everything else already fetched.
type BackfillResult struct {
	InstID  string `json:"instId"`
	Bar     string `json:"bar"`
	Fetched int    `json:"fetched"`
	Stored  int    `json:"stored"`
	// Oldest/Newest bound what was actually retrieved, which is how a caller sees that a pair
	// returned less history than asked for. Pointers so a pair that fetched nothing omits them
	// entirely rather than serializing a zero time — a JSON "0001-01-01T00:00:00Z" renders as a
	// real date in a panel, which is worse than an absent field.
	Oldest *time.Time `json:"oldest,omitempty"`
	Newest *time.Time `json:"newest,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// Run fetches history for every (instrument, bar) pair in req and writes it to the repository.
//
// Resumable and safe to re-run: SaveCandle upserts on (inst_id, bar, ts), so re-running overwrites
// identical rows rather than duplicating them, and a run interrupted halfway can simply be issued
// again. It does NOT skip pairs that already have data — a caller wanting more history than a
// previous run fetched must be able to ask for it, and the upsert makes the overlap free.
//
// Returns one result per pair, plus an error only if the request itself is unusable. A pair that
// fails carries its error in its own result.
func (b *Backfill) Run(ctx context.Context, req BackfillRequest) ([]BackfillResult, error) {
	logger := b.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if len(req.InstIDs) == 0 || len(req.Bars) == 0 {
		return nil, fmt.Errorf("backfill requires at least one instrument and one bar")
	}
	target := req.TargetCandles
	if target <= 0 {
		target = DefaultTargetCandles
	}
	delay := b.PageDelay
	if delay <= 0 {
		delay = DefaultPageDelay
	}

	results := make([]BackfillResult, 0, len(req.InstIDs)*len(req.Bars))
	for _, instID := range req.InstIDs {
		for _, bar := range req.Bars {
			res := b.runPair(ctx, instID, bar, target, delay, logger)
			results = append(results, res)
			if ctx.Err() != nil {
				// Cancellation stops the run but keeps what has already been fetched: those rows
				// are durably stored and a later run resumes from them.
				return results, ctx.Err()
			}
		}
	}
	return results, nil
}

func (b *Backfill) runPair(ctx context.Context, instID, bar string, target int, delay time.Duration, logger *slog.Logger) BackfillResult {
	res := BackfillResult{InstID: instID, Bar: bar}

	// Walk backwards from now: each page returns candles older than the previous page's oldest.
	var before time.Time
	seen := make(map[int64]struct{}, target)

	for res.Fetched < target {
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			return res
		}

		page, err := b.Exchange.GetHistoryCandles(instID, bar, before, 0)
		if err != nil {
			res.Error = fmt.Sprintf("fetch page before %v: %v", before, err)
			logger.Warn("backfill page failed", "instId", instID, "bar", bar, "before", before, "error", err)
			return res
		}
		if len(page) == 0 {
			// No more history available. Normal for a recently-listed instrument or a long
			// timeframe, not an error — the pair simply has less history than requested.
			logger.Info("backfill reached end of available history",
				"instId", instID, "bar", bar, "fetched", res.Fetched, "target", target)
			break
		}

		oldest := page[0].Timestamp
		progressed := false
		for _, c := range page {
			ts := c.Timestamp.UnixMilli()
			if _, dup := seen[ts]; dup {
				continue
			}
			seen[ts] = struct{}{}
			progressed = true

			if c.Timestamp.Before(oldest) {
				oldest = c.Timestamp
			}
			ts2 := c.Timestamp
			if res.Newest == nil || ts2.After(*res.Newest) {
				res.Newest = &ts2
			}
			if res.Oldest == nil || ts2.Before(*res.Oldest) {
				res.Oldest = &ts2
			}

			res.Fetched++
			if err := b.Repo.SaveCandle(ctx, port.Candle{InstID: instID, Bar: bar, Candle: c}); err != nil {
				// Keep going: one bad row should not discard an otherwise good page, and the
				// upsert means a retry can fill the gap later.
				logger.Warn("backfill store failed", "instId", instID, "bar", bar, "ts", c.Timestamp, "error", err)
				continue
			}
			res.Stored++
		}

		if !progressed {
			// Every candle in this page was already seen, so the cursor is not advancing. Without
			// this guard the loop would page forever against an endpoint that keeps returning the
			// same window.
			logger.Warn("backfill cursor stalled, stopping", "instId", instID, "bar", bar, "before", before)
			break
		}
		before = oldest
		time.Sleep(delay)
	}

	logger.Info("backfill pair complete", "instId", instID, "bar", bar,
		"fetched", res.Fetched, "stored", res.Stored, "oldest", res.Oldest, "newest", res.Newest)
	return res
}
