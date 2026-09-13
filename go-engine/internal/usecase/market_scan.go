// Package-level note: this file is the token-discovery scan (2026-09-13 request — "find top tokens
// from exchanges: top 10, high volume, highest changes, trends").
//
// It is deliberately NOT its own service/container, on the operator's own reasoning: it serves
// nothing to anyone, it runs a few times a day rather than continuously, and all it does is search
// for tokens and add them to the main services' roster. cmd/api hosts it as a scheduled job —
// cmd/api already holds the exchange clients and the database, so a separate container would add a
// deployment unit, a memory footprint on a 3.9GB box (§35.7), and nothing else.
package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// allTickerFetcher is the one exchange capability the scan needs: the whole market in one call.
//
// Narrow on purpose, the same reasoning as port.HistoryCandleFetcher (§17) and cmd/api's
// positionLister (§48): a discovery job must not be able to place or cancel an order, and the type
// system is a better guarantee of that than care. Both rest.Client types satisfy it already.
type allTickerFetcher interface {
	GetAllTickers(instType string) ([]domain.MarketTicker, error)
}

// ExchangeSource is one exchange the scan covers. The list comes from config today
// (config.Scan.Exchanges) and is shaped to move to a database table later without touching the
// scanner: everything below reads this struct, never config.
type ExchangeSource struct {
	// Name is the exchange identity stored on every instruments/market_tokens row ("okx", "mexc").
	Name   string
	Client allTickerFetcher
	// InstType is the product family to scan — "FUTURES" for OKX (where the X-Perp perpetuals real
	// trading executes against live, §33.2), ignored by MEXC, which has exactly one futures family.
	InstType string
	// QuoteSuffixes are the wire-format id patterns that identify a USD-quoted perpetual on this
	// exchange, and the text stripped to recover the short internal symbol. Required: an exchange
	// with no entry scans nothing rather than guessing at its naming, since a wrong guess silently
	// admits dated futures or spot pairs to a perpetual-futures bot.
	QuoteSuffixes []string
}

// Scan weights. Deliberately a small, explicit set rather than a tuned model: the scan's job is to
// surface candidates for a human (and for paper trading) to judge, not to predict returns.
//
// Volume dominates because it is the one property that actually gates whether this bot can trade a
// token at all — a thin market's spread and slippage make an otherwise good signal unprofitable, and
// §33.2's own finding was that the execution venue's liquidity, not the signal, was the binding
// constraint. Change and range are opportunity proxies on top of that, not substitutes for it.
var (
	scanWeightVolume = decimal.NewFromFloat(0.55)
	scanWeightChange = decimal.NewFromFloat(0.25)
	scanWeightRange  = decimal.NewFromFloat(0.20)
)

// MinScanVolumeUSD is the floor below which a token is not a candidate at any score. A market doing
// under this in a day cannot absorb even this project's small positions without moving, so ranking
// such a token highly on a big percentage move would be actively misleading.
var MinScanVolumeUSD = decimal.NewFromInt(1_000_000)

// ScoreToken reduces one market snapshot to a single comparable number in roughly [0,1].
//
// Each component is normalized against the SCAN's own maximum rather than an absolute constant:
// "high volume" only means anything relative to the rest of the market that day, and a fixed
// divisor would need re-tuning every time the market's overall activity shifted. maxVol must be the
// largest Vol24hUSD in the batch.
//
// Change is taken as an ABSOLUTE value: a token down 30% is as tradeable as one up 30% for a bot
// that opens both sides (§9), and signing it would rank the whole market by direction, which is a
// prediction the scan has no business making.
func ScoreToken(t domain.MarketTicker, maxVol decimal.Decimal) decimal.Decimal {
	var volPart, changePart, rangePart decimal.Decimal

	if maxVol.IsPositive() {
		// Ratio of the day's largest market. Linear rather than logarithmic on purpose: the aim is
		// to separate the genuinely liquid handful from everything else, and a log scale compresses
		// exactly that gap.
		volPart = t.Vol24hUSD.Div(maxVol)
		if volPart.GreaterThan(decimal.NewFromInt(1)) {
			volPart = decimal.NewFromInt(1)
		}
	}

	// 10% in a day is treated as a full-strength move; beyond that the component saturates rather
	// than letting one 400% meme-coin spike dominate the ranking for every other token.
	changePart = t.Change24hPct.Abs().Div(decimal.NewFromInt(10))
	if changePart.GreaterThan(decimal.NewFromInt(1)) {
		changePart = decimal.NewFromInt(1)
	}

	if r := RangePct(t); r.IsPositive() {
		// Same saturation, at a 10% intraday range.
		rangePart = r.Div(decimal.NewFromInt(10))
		if rangePart.GreaterThan(decimal.NewFromInt(1)) {
			rangePart = decimal.NewFromInt(1)
		}
	}

	return volPart.Mul(scanWeightVolume).
		Add(changePart.Mul(scanWeightChange)).
		Add(rangePart.Mul(scanWeightRange)).
		Round(6)
}

// RangePct is the 24h high-low spread as a percentage of the last price — a volatility proxy that,
// unlike 24h change, does NOT cancel out on a token that moved hard in both directions and came
// back. Those are the days a mean-reversion or breakout strategy has the most to work with, and a
// change-only ranking is blind to them.
func RangePct(t domain.MarketTicker) decimal.Decimal {
	if !t.Last.IsPositive() {
		return decimal.Zero
	}
	return t.High24h.Sub(t.Low24h).Div(t.Last).Mul(decimal.NewFromInt(100))
}

// SymbolFromInstID recovers the short internal symbol ("BTC") from an exchange's wire-format id,
// returning ok=false when the id is not a USD-quoted perpetual on this exchange.
//
// Returning false rather than a best guess is the important part. On OKX, instType=FUTURES contains
// 179 X-Perp perpetuals AND 28 DATED futures (live-checked: eleven BTC contracts alone, e.g.
// BTC-USD-270924) — admitting a dated contract to a perpetual-futures bot would place real orders on
// an instrument that expires, and its id differs from the perpetual's only in the part being
// stripped. A silent wrong answer here is worse than no answer.
func SymbolFromInstID(instID string, suffixes []string) (string, bool) {
	for _, suf := range suffixes {
		// A prefix match, not Contains: the marker has to sit where the quote currency belongs, or
		// "BTC-USD-270924" and "BTC-USD_UM_XPERP-310404" become indistinguishable.
		idx := strings.Index(instID, suf)
		if idx <= 0 {
			continue
		}
		sym := instID[:idx]
		// What actually rejects a dated contract is the suffix match itself, not this guard:
		// live-checked against the real response, the three configured OKX patterns accept exactly
		// the 179 X-Perp perpetuals and reject all 28 dated ones ("BTC-USD-270924",
		// "BTC-USD_UM-260925" — neither contains a configured suffix at all).
		//
		// This guard is a second, cheaper line against a MISCONFIGURED suffix list rather than
		// against OKX's current naming: a pattern short enough to match mid-id would otherwise
		// yield a symbol carrying a separator, which is never a real token name. It does not fire
		// on any live instrument today, and is kept as a cheap invariant rather than claimed as the
		// dated-futures defence.
		if sym == "" || strings.ContainsAny(sym, "-_") {
			continue
		}
		return sym, true
	}
	return "", false
}

// MarketScanner finds tradeable tokens across every configured exchange and admits the best of them
// to the roster. See this file's package comment for why it is a job rather than a service.
type MarketScanner struct {
	Repo      port.Repository
	Exchanges []ExchangeSource
	Logger    *slog.Logger

	// TopN is how many of each exchange's ranked candidates are ADMITTED to the roster per scan.
	// Bounded deliberately: every admitted token costs a WebSocket subscription, a candle window per
	// timeframe, and a share of the shared account's equity through dynamic sizing (§32.4), so an
	// unbounded "add everything good" would dilute every position's size toward nothing.
	TopN int
	// MinVolumeUSD overrides MinScanVolumeUSD when positive.
	MinVolumeUSD decimal.Decimal
}

// ScanResult reports one exchange's outcome. Per-exchange rather than aggregate so one exchange
// failing is visible as itself rather than as a smaller total — the same reasoning as §17's
// per-instrument BackfillResult.
type ScanResult struct {
	Exchange   string
	Scanned    int // instruments returned by the exchange
	Candidates int // USD-quoted perpetuals clearing the volume floor
	Admitted   int // roster rows created or refreshed
	Err        error
}

// Scan fetches every configured exchange's whole market, stores the ranked snapshot, and admits the
// top candidates to the roster.
//
// Partial failure is expected, not fatal: one exchange being unreachable must leave the others'
// results intact, and it must NOT clear that exchange's last known-good snapshot either — an
// exchange returning nothing is a fault, and deleting the only view of its market on a fault is the
// opposite of useful (ReplaceMarketTokens declines an empty write for exactly this).
func (s *MarketScanner) Scan(ctx context.Context) []ScanResult {
	out := make([]ScanResult, 0, len(s.Exchanges))
	for _, ex := range s.Exchanges {
		out = append(out, s.scanOne(ctx, ex))
	}
	return out
}

func (s *MarketScanner) scanOne(ctx context.Context, ex ExchangeSource) ScanResult {
	res := ScanResult{Exchange: ex.Name}

	tickers, err := ex.Client.GetAllTickers(ex.InstType)
	if err != nil {
		res.Err = fmt.Errorf("fetch tickers: %w", err)
		return res
	}
	res.Scanned = len(tickers)

	candidates := s.candidates(ex, tickers)
	res.Candidates = len(candidates)
	if len(candidates) == 0 {
		// Not an error: a misconfigured suffix list or a genuinely quiet market both land here, and
		// the counts above are what distinguish them (Scanned>0 with Candidates==0 means the
		// filtering is wrong, not the market).
		return res
	}

	maxVol := candidates[0].Vol24hUSD
	for _, c := range candidates {
		if c.Vol24hUSD.GreaterThan(maxVol) {
			maxVol = c.Vol24hUSD
		}
	}

	toks := make([]port.MarketToken, 0, len(candidates))
	for _, c := range candidates {
		sym, ok := SymbolFromInstID(c.InstID, ex.QuoteSuffixes)
		if !ok {
			continue
		}
		toks = append(toks, port.MarketToken{
			Exchange: ex.Name, Symbol: sym, ExecInstID: c.InstID,
			LastPx: c.Last, Open24h: c.Open24h, High24h: c.High24h, Low24h: c.Low24h,
			Vol24hUSD: c.Vol24hUSD, Change24hPct: c.Change24hPct,
			Range24hPct: RangePct(c), Score: ScoreToken(c, maxVol),
		})
	}
	sort.Slice(toks, func(i, j int) bool { return toks[i].Score.GreaterThan(toks[j].Score) })

	if err := s.Repo.ReplaceMarketTokens(ctx, ex.Name, toks); err != nil {
		res.Err = fmt.Errorf("store snapshot: %w", err)
		return res
	}

	res.Admitted = s.admit(ctx, ex, toks)
	return res
}

// candidates filters the raw market down to USD-quoted perpetuals on this exchange that clear the
// volume floor and carry a usable price.
func (s *MarketScanner) candidates(ex ExchangeSource, tickers []domain.MarketTicker) []domain.MarketTicker {
	floor := s.MinVolumeUSD
	if !floor.IsPositive() {
		floor = MinScanVolumeUSD
	}
	out := make([]domain.MarketTicker, 0, len(tickers))
	for _, t := range tickers {
		if _, ok := SymbolFromInstID(t.InstID, ex.QuoteSuffixes); !ok {
			continue
		}
		// A zero price is the never-traded instrument OKX reports with empty price fields
		// (okx.LooseDecimal's comment) — it has no market to rank.
		if !t.Last.IsPositive() || t.Vol24hUSD.LessThan(floor) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// admit writes the top candidates into the roster.
//
// The per-mode flags encode the operator's own instruction (2026-09-13): a discovered token joins
// the WebSocket subscriptions and paper trading straight away — that is how it earns a track record
// — and is added to the real-mode list DISABLED, for a person to turn on. Discovery and real-capital
// exposure stay separate decisions.
//
// Note UpsertInstrument does not touch an existing row's flags, so re-finding a token an operator
// disabled leaves it disabled; only its market snapshot is refreshed.
func (s *MarketScanner) admit(ctx context.Context, ex ExchangeSource, toks []port.MarketToken) int {
	limit := s.TopN
	if limit <= 0 || limit > len(toks) {
		limit = len(toks)
	}
	var n int
	for _, t := range toks[:limit] {
		in := port.Instrument{
			Symbol: t.Symbol, Exchange: ex.Name, ExecInstID: t.ExecInstID, InstType: ex.InstType,
			EnabledIngest: true,
			EnabledPaper:  true,
			EnabledReal:   false,
			Source:        "scan",
			Vol24hUSD:     t.Vol24hUSD, Change24hPct: t.Change24hPct, ScanScore: t.Score,
		}
		if _, err := s.Repo.UpsertInstrument(ctx, in); err != nil {
			// One token failing must not abandon the rest — the same partial-failure posture as the
			// per-exchange loop above.
			s.log().Warn("scan: could not admit token to roster",
				"exchange", ex.Name, "symbol", t.Symbol, "err", err)
			continue
		}
		n++
	}
	return n
}

// RunEvery runs Scan on a ticker until ctx is done, starting with one immediate scan so a freshly
// started cmd/api has a market snapshot without waiting out the first interval (a panel showing an
// empty market for hours after a deploy would read as a broken feature).
//
// The interval is hours, not minutes, by explicit decision: the roster changes on the order of days,
// and a scan is two whole-market REST calls against a budget shared with the live trading path
// (§27.1/§39's own finding that redundant polling is what rate-limited the account).
func (s *MarketScanner) RunEvery(ctx context.Context, every time.Duration) {
	if every <= 0 {
		s.log().Info("market scan disabled (interval not positive)")
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.logResults(s.Scan(ctx))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *MarketScanner) logResults(results []ScanResult) {
	for _, r := range results {
		if r.Err != nil {
			s.log().Error("market scan failed", "exchange", r.Exchange, "err", r.Err)
			continue
		}
		s.log().Info("market scan complete", "exchange", r.Exchange,
			"scanned", r.Scanned, "candidates", r.Candidates, "admitted", r.Admitted)
	}
}

func (s *MarketScanner) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
