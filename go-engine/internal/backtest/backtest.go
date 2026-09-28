// Package backtest replays stored candles to build the RL model's warm-start training set
// (docs/RL_V8_PLAN.md).
//
// WHY THIS EXISTS. Starting a policy from random initialization produces a structural deadlock
// (§16.9): an untrained policy skips essentially every signal, so nothing opens, so nothing closes,
// so no reward arrives, so the weights never change. §15.8 removed the previous warm start after it
// diverged — but the diagnosis there was specific: it trained against a dataset with almost no real
// strategy-signal history, which is the thin condition a replay phase would always start from at
// this project's data volume. That limit no longer holds. Strategies are Go code, so they can be
// run over stored candles to produce REAL signals from the same implementations production uses.
//
// WHAT MAKES THIS FAITHFUL, and why it is not just "a backtest":
//
//   - The strategies are the real `strategy.Strategy` values, evaluated through the same
//     EvaluateWith production calls. No reimplementation, the §16.2 precedent.
//   - The observations are built by the same `usecase.BuildMarketBlock` / `BuildBTCContext` /
//     `BuildIndicators` the live engine uses. A separate builder here would be a second definition
//     of the model's input, free to disagree with the one being served — the exact train/serve skew
//     v8 exists to remove (§19.1 records what one costs).
//   - The reward is `rl_service/reward.py`'s formula, ported once and pinned by tests against the
//     same numbers, because the whole point is that a warm-started policy is scored the same way
//     live trading will score it.
//
// WHAT IT IS NOT. This is a warm start, not a replacement for forward-test learning (§2). It exists
// so the policy is not random on its first live decision; real training continues from live
// outcomes afterwards.
package backtest

import (
	"context"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// Sample is one completed trade: the observation the decision was made from, and what happened.
//
// Written as JSONL for the trainer to consume. The observation is the FULL domain.Observation
// rather than a pre-flattened vector, deliberately: flattening here would bake this binary's idea
// of the layout into the dataset, and a dataset that outlives one schema version is worth more than
// one that must be regenerated whenever a field moves.
type Sample struct {
	Observation domain.Observation `json:"observation"`
	// Terminal is the same observation shaped as the closing call — the reward-delivery shape the
	// live path sends (§15.10), carried so the trainer can pair a decision with its outcome exactly
	// as the learner does at runtime.
	Terminal domain.Observation `json:"terminal"`

	Reward      float64 `json:"reward"`
	RealizedPnL float64 `json:"realized_pnl_usd"`
	CloseReason string  `json:"close_reason"`

	// Diagnostics, not model inputs. A dataset nobody can inspect is a dataset nobody can trust.
	InstID      string    `json:"inst_id"`
	Bar         string    `json:"bar"`
	Kind        string    `json:"kind"`
	Side        string    `json:"side"`
	OpenedAt    time.Time `json:"opened_at"`
	ClosedAt    time.Time `json:"closed_at"`
	EntryPx     float64   `json:"entry_px"`
	ExitPx      float64   `json:"exit_px"`
	HoldBars    int       `json:"hold_bars"`
	Adjustments int       `json:"sltp_adjustments"`
}

// Config parameterizes one run.
type Config struct {
	// Exchange selects which exchange's candles this run replays ("okx", "mexc", ...). Empty
	// defaults to "okx" (loadCandles' own default) — this package started OKX-only, and the
	// default keeps every existing caller unaffected. Every instrument in InstIDs, and BTC's own
	// reference series, are read from this one exchange; a run cannot mix exchanges (matching how
	// every other per-exchange concern in this codebase — paper_trading_config, strategy_
	// assignments, account_equity — is scoped one exchange at a time, migration 000037/000038).
	Exchange string
	InstIDs  []string
	Bars     []string
	// Kinds to evaluate. Empty means every registered strategy kind — appropriate for the dataset,
	// where no capital is at risk, unlike the live roster which is deliberately small.
	Kinds []string

	From time.Time
	To   time.Time

	// Account is the simulated account every instrument trades against, mirroring the live shared
	// account (§15.6) rather than giving each token its own — a per-token account would teach the
	// policy that losses elsewhere do not constrain it, which is false.
	InitialUSD    decimal.Decimal
	MaxLeverage   decimal.Decimal
	PositionSlots int

	// MaxPositionPct caps one position as a fraction of equity, the same ceiling production applies
	// after the even split (§15.6). Zero disables it, which is only sensible in a test.
	MaxPositionPct decimal.Decimal

	// Clamps bound where levels may be placed, exactly as production does (§19.2, §45). Training
	// without them would let the policy learn placements Go silently rejects.
	Clamps conductor.Clamps

	// CandleWindow is how many candles of history each evaluation sees, matching
	// paper_trading.candle_limit.
	CandleWindow int

	// Params overrides strategy parameters across the run, applied through each kind's own
	// WithParams so a kind ignores names it does not declare. Nil leaves every default in place.
	Params map[string]decimal.Decimal
}

// Result summarizes one run, for the operator to read before anything is trained on it.
type Result struct {
	Samples    int     `json:"samples"`
	Wins       int     `json:"wins"`
	Losses     int     `json:"losses"`
	WinRate    float64 `json:"win_rate"`
	TotalPnL   float64 `json:"total_pnl_usd"`
	MeanReward float64 `json:"mean_reward"`
	// Per-reason counts, because "how did these trades end" is the first question worth asking of a
	// dataset and an aggregate cannot answer it.
	ByReason map[string]int `json:"by_reason"`
	// ByStrategy breaks the run down per kind, which is the whole point of running one: an
	// aggregate win rate over 26 strategies says only that the MIX loses money, and cannot say
	// which of them to keep. §15.5 makes the same argument for per-token reward breakdowns — "good
	// on average, bad for one" is invisible in a total.
	ByStrategy map[string]*KindStats `json:"by_strategy"`

	// Significance compares every strategy against the null baseline, when one was included in the
	// run. Reported because a raw ranking is misleading: the first full screening's entire table of
	// 37 strategies had every gap at t < 1, so the ordering carried no information at all.
	Significance []Significance `json:"significance,omitempty"`

	// Resets counts how many times the simulated account was drained and topped back up (§15.7).
	// Surfaced because a dataset built across many resets describes a strategy mix that loses
	// money, and that is the first thing worth knowing before training on it.
	Resets int `json:"account_resets"`

	// Skipped counts signals that produced no sample, by why. A run that discards most of its
	// signals is not a run to train on, and without this it would look identical to a quiet market.
	Skipped map[string]int `json:"skipped"`

	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// KindStats is one strategy's record over a run.
type KindStats struct {
	Kind       string  `json:"kind"`
	Trades     int     `json:"trades"`
	Wins       int     `json:"wins"`
	WinRate    float64 `json:"win_rate"`
	PnLUSD     float64 `json:"pnl_usd"`
	MeanReward float64 `json:"mean_reward"`
	// AvgHoldBars separates a scalp from a swing held for days — two strategies with the same win
	// rate and very different capital turnover are not equally useful.
	AvgHoldBars float64 `json:"avg_hold_bars"`
	// PnLPerTrade is what ranks them: total PnL rewards whichever kind simply traded most.
	PnLPerTrade float64 `json:"pnl_per_trade"`

	holdSum   int
	rewardSum float64
}

// CandleSource reads stored history. Narrower than port.Repository on purpose: a dataset builder
// must not be able to open an order, and the type system should enforce that rather than the
// implementation being careful — the same reasoning as §17's HistoryCandleFetcher.
type CandleSource interface {
	ListCandlesRange(ctx context.Context, exchange, instID, bar string, from, to time.Time, limit int) ([]port.Candle, error)
	CandleRange(ctx context.Context, exchange, instID, bar string) (oldest, newest time.Time, err error)
}

// Sink receives each completed sample. An interface so a run can stream to a file without holding
// the whole dataset in memory — a multi-month replay across every strategy is large.
type Sink interface {
	Write(Sample) error
}

// loadCandles reads a full range in pages, oldest first. exchange defaults to "okx" when empty —
// this package started OKX-only (docs/RL_V8_PLAN.md's warm-start dataset), and the default keeps
// every existing caller (cmd/backtest) byte-identical while a caller that DOES care about a
// second exchange (the strategy optimizer, 2026-09-27 — "هرچی که میسازیم باید برای تمام صرافی ها
// کار بکنه") can set Config.Exchange explicitly.
func loadCandles(ctx context.Context, src CandleSource, exchange, instID, bar string, from, to time.Time) ([]domain.Candle, error) {
	if exchange == "" {
		exchange = "okx"
	}
	const page = 5000
	var out []domain.Candle
	cursor := from
	for {
		rows, err := src.ListCandlesRange(ctx, exchange, instID, bar, cursor, to, page)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			out = append(out, r.Candle)
		}
		last := rows[len(rows)-1].Candle.Timestamp
		if !last.After(cursor) {
			// The cursor stopped advancing, which would loop forever. Can only happen on duplicate
			// timestamps, which the (inst_id, bar, ts) primary key forbids — guarded anyway, since
			// an infinite loop over a database is a worse failure than a short read.
			break
		}
		cursor = last.Add(time.Nanosecond)
		if len(rows) < page {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

// stratRecord accumulates a strategy's realized record DURING the run, so the observation's
// strategy-profile block carries what the simulation itself has learned so far.
//
// This is what makes building NEW trades work where rebuilding the existing paper_orders rows does
// not: a historical row needs the database's state at a past instant, which no longer exists, while
// a forward simulation always knows its own books. It starts from zero, exactly like a fresh install.
type stratRecord struct {
	trades  int
	wins    int
	pnlSum  decimal.Decimal
	holdSum time.Duration
	rrSum   decimal.Decimal
}

func (r stratRecord) profile(kind, bar string) domain.StrategySignal {
	sig := domain.StrategySignal{Kind: kind, Bar: bar, TradeCount: r.trades}
	if r.trades == 0 {
		return sig
	}
	n := decimal.NewFromInt(int64(r.trades))
	sig.WinRate = decimal.NewFromInt(int64(r.wins)).Div(n)
	sig.AvgPnLPerTrade = r.pnlSum.Div(n)
	sig.AvgRR = r.rrSum.Div(n)
	sig.AvgHoldHours = decimal.NewFromFloat(r.holdSum.Hours() / float64(r.trades))
	return sig
}

// position is one open simulated trade.
type position struct {
	instID   string
	bar      string
	kind     string
	side     string
	entryPx  decimal.Decimal
	slPx     *decimal.Decimal
	tpPx     *decimal.Decimal
	size     decimal.Decimal
	leverage decimal.Decimal

	openedAt  time.Time
	openedIdx int
	openObs   domain.Observation

	pnlMaxPct   decimal.Decimal
	pnlMinPct   decimal.Decimal
	adjustments int

	// updates holds the `update` observations emitted while this position was open, waiting for
	// the reward. They cannot be written when they happen: an update's answer is judged by what the
	// trade went on to do, and that is only known at close — the same pairing §15.11 describes for
	// the live learner, where a decision is held pending until its outcome lands hours later.
	updates []domain.Observation
	// lastUpdatePnL/lastUpdateAt are the cadence baseline, mirroring conductor.ShouldUpdate: an
	// update fires once PnL has moved past the threshold OR the time ceiling has elapsed.
	lastUpdatePnL decimal.Decimal
	lastUpdateAt  time.Time
}

// evaluate runs one strategy over a candle window, returning its signal.
//
// The real strategy value through the real dispatch function — the point of the whole design. A
// reimplementation here would drift from production the moment either changed, which is §16.2's
// stated reason for keeping trial evaluation in Go rather than Python.
func evaluate(s strategy.Strategy, view strategy.MarketView) (strategy.Signal, bool) {
	sig, err := strategy.EvaluateWith(s, view)
	if err != nil || sig.Side == strategy.Hold {
		return strategy.Signal{}, false
	}
	return sig, true
}

// touchReason judges a bar against an open position's levels.
//
// SL WINS A TIE. When a bar's high and low span both levels, the simulation cannot know which came
// first, and assuming TP would record a LOSING trade as a win: in reality the stop fired, the
// position was already closed, and the high it later printed was never available to it. That error
// is one-directional and always optimistic — it teaches the policy that tight stops are safe, which
// is exactly the belief that costs real money. Measured on live data, stops are not rare: 1315 SL
// closes against 411 TP closes, with reward:risk capped at 3 so the stop is nearer by construction.
func touchReason(side string, slPx, tpPx *decimal.Decimal, c domain.Candle) (string, decimal.Decimal, bool) {
	hitSL := slPx != nil && ((side == "buy" && c.Low.LessThanOrEqual(*slPx)) ||
		(side == "sell" && c.High.GreaterThanOrEqual(*slPx)))
	if hitSL {
		return "sl", *slPx, true
	}
	hitTP := tpPx != nil && ((side == "buy" && c.High.GreaterThanOrEqual(*tpPx)) ||
		(side == "sell" && c.Low.LessThanOrEqual(*tpPx)))
	if hitTP {
		return "tp", *tpPx, true
	}
	return "", decimal.Zero, false
}

// pnlPct is a position's PnL as a fraction of margin — entry-to-price scaled by leverage, matching
// usecase.unrealizedPnLPct so the simulation and production agree on what a percentage means
// (§19.1 records the cost of the two disagreeing).
func pnlPct(p *position, price decimal.Decimal) decimal.Decimal {
	if !p.entryPx.IsPositive() {
		return decimal.Zero
	}
	dir := decimal.NewFromInt(1)
	if p.side == "sell" {
		dir = decimal.NewFromInt(-1)
	}
	lev := p.leverage
	if !lev.IsPositive() {
		lev = decimal.NewFromInt(1)
	}
	return dir.Mul(price.Sub(p.entryPx)).Div(p.entryPx).Mul(lev)
}

// realizedPnL is the dollar outcome, net of fees.
func realizedPnL(p *position, exit decimal.Decimal) (pnl, fees decimal.Decimal) {
	gross := pnlPct(p, exit).Mul(p.size)
	// Taker both ways, at OKX's published rate. Charged because the live reward charges it: a
	// dataset scored without fees would teach the policy that a round trip is free, and at these
	// position sizes fees are a meaningful fraction of a typical outcome.
	notional := p.size.Mul(p.leverage)
	fees = notional.Mul(takerFeeRate).Mul(decimal.NewFromInt(2))
	return gross.Sub(fees), fees
}

// takerFeeRate is OKX's taker fee for perpetual futures. A constant rather than config: it is an
// exchange fact, and a wrong value here would silently bias every sample in the dataset.
var takerFeeRate = decimal.NewFromFloat(0.0005)

// riskPct mirrors usecase.riskPct — the entry-to-stop distance scaled by leverage, which is what the
// reward divides by.
func riskPct(p *position) decimal.Decimal {
	if p.slPx == nil || !p.entryPx.IsPositive() {
		return decimal.Zero
	}
	lev := p.leverage
	if !lev.IsPositive() {
		lev = decimal.NewFromInt(1)
	}
	return p.entryPx.Sub(*p.slPx).Abs().Div(p.entryPx).Mul(lev)
}
