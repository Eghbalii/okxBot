package backtest

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSource serves candles from memory, filtering the same way the real query does.
type fakeSource struct {
	series map[string][]domain.Candle // keyed "instID/bar"
}

func (f *fakeSource) ListCandlesRange(_ context.Context, instID, bar string, from, to time.Time, limit int) ([]port.Candle, error) {
	var out []port.Candle
	for _, c := range f.series[instID+"/"+bar] {
		if !from.IsZero() && c.Timestamp.Before(from) {
			continue
		}
		if !to.IsZero() && !c.Timestamp.Before(to) {
			continue
		}
		out = append(out, port.Candle{InstID: instID, Bar: bar, Candle: c})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeSource) CandleRange(_ context.Context, instID, bar string) (time.Time, time.Time, error) {
	s := f.series[instID+"/"+bar]
	if len(s) == 0 {
		return time.Time{}, time.Time{}, nil
	}
	return s[0].Timestamp, s[len(s)-1].Timestamp, nil
}

// trendingSeries builds candles that rise and fall enough for real strategies to fire on them.
//
// Deliberately not flat and not monotonic: §30.1 records a whole test suite passing vacuously
// against a fixture whose candles had zero-height bodies, so every strategy returned Hold and the
// suite compared Hold against Hold from end to end while reporting PASS.
func trendingSeries(n int, base float64) []domain.Candle {
	out := make([]domain.Candle, 0, n)
	px := base
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		// A slow trend with a real oscillation on top, so breakouts, pullbacks and reversals all
		// occur somewhere in the series.
		swing := 0.004 * float64((i%17)-8)
		drift := 0.0006 * float64((i/40)%5-2)
		px *= 1 + swing + drift
		c := decimal.NewFromFloat(px)
		out = append(out, domain.Candle{
			Timestamp: start.Add(time.Duration(i) * 5 * time.Minute),
			Open:      c.Mul(dec("0.9985")),
			High:      c.Mul(dec("1.004")),
			Low:       c.Mul(dec("0.996")),
			Close:     c,
			Volume:    decimal.NewFromFloat(1000 + float64((i*37)%500)),
		})
	}
	return out
}

func testRunner(src *fakeSource, sink Sink, kinds []string) *Runner {
	return &Runner{
		Cfg: Config{
			InstIDs:        []string{"SOL"},
			Bars:           []string{"5m"},
			Kinds:          kinds,
			InitialUSD:     dec("40"),
			MaxLeverage:    dec("10"),
			PositionSlots:  16,
			MaxPositionPct: dec("0.25"),
			CandleWindow:   300,
			Clamps: conductor.Clamps{
				MinSLDistPct: dec("0.002"),
				MaxSLDistPct: dec("0.05"),
				MinTPSLRatio: dec("1.5"),
				MaxTPSLRatio: dec("3"),
				MaxLossPct:   dec("0.15"),
			},
		},
		Src:    src,
		Sink:   sink,
		Logger: testLogger(),
	}
}

func fixture(kinds []string) (*fakeSource, *MemorySink, *Runner) {
	src := &fakeSource{series: map[string][]domain.Candle{
		"SOL/5m": trendingSeries(600, 150),
		"BTC/5m": trendingSeries(600, 64000),
	}}
	sink := &MemorySink{}
	return src, sink, testRunner(src, sink, kinds)
}

// The run must actually produce trades. A backtest that silently yields nothing looks identical to
// a quiet market, which is how §30.1's vacuous suite passed for weeks.
func TestRun_ProducesSamplesFromRealStrategies(t *testing.T) {
	_, sink, r := fixture([]string{"range_breakout_v2", "stepped_trailing", "macd_momentum"})

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Samples == 0 {
		t.Fatalf("no samples produced; skipped=%v", res.Skipped)
	}
	if len(sink.Samples) != res.Samples {
		t.Errorf("sink holds %d samples but the result claims %d", len(sink.Samples), res.Samples)
	}
}

// Every sample's observation must be one the model could actually be served.
//
// The single most important property here: a warm start built on observations production would
// refuse is a warm start on a different input distribution, which is worse than none at all.
func TestRun_EverySampleObservationIsValid(t *testing.T) {
	_, sink, r := fixture(nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.Samples) == 0 {
		t.Fatal("no samples to check")
	}
	for i, s := range sink.Samples {
		if err := s.Observation.Validate(); err != nil {
			t.Fatalf("sample %d's decision observation is invalid: %v", i, err)
		}
		if err := s.Terminal.Validate(); err != nil {
			t.Fatalf("sample %d's terminal observation is invalid: %v", i, err)
		}
		if s.Terminal.Category == s.Observation.Category {
			t.Fatalf("sample %d's terminal call carries the opening category %q", i, s.Terminal.Category)
		}
	}
}

// A trade's outcome must stay attached to the decision that produced it (§15.10) — the terminal
// call deliberately does NOT zero entry/SL/TP, or the model could not learn which placement caused
// which result.
func TestRun_TerminalKeepsTheDecisionsLevels(t *testing.T) {
	_, sink, r := fixture([]string{"range_breakout_v2"})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.Samples) == 0 {
		t.Skip("no samples")
	}
	op := opens(sink.Samples)
	if len(op) == 0 {
		t.Skip("no open decisions")
	}
	s := op[0]
	if s.Terminal.Signal == nil || s.Observation.Signal == nil {
		t.Fatal("both calls must carry the signal")
	}
	if !s.Terminal.Signal.SLPx.Equal(s.Observation.Signal.SLPx) {
		t.Errorf("terminal SL %s != decision SL %s", s.Terminal.Signal.SLPx, s.Observation.Signal.SLPx)
	}
	if !s.Terminal.PositionState.RealizedPnLUSD.Equal(decimal.NewFromFloat(s.RealizedPnL)) {
		t.Errorf("terminal PnL %s does not match the sample's %v",
			s.Terminal.PositionState.RealizedPnLUSD, s.RealizedPnL)
	}
}

// When a bar spans both levels, SL wins.
//
// Assuming TP would record a LOSING trade as a win: in reality the stop fired, the position was
// already closed, and the high it later printed was never available to it. The error is
// one-directional and always optimistic — it teaches the policy that tight stops are safe, which is
// exactly the belief that costs real money.
func TestTouchReason_StopWinsWhenABarSpansBoth(t *testing.T) {
	sl, tp := dec("95"), dec("110")
	spanning := domain.Candle{Open: dec("100"), High: dec("115"), Low: dec("90"), Close: dec("112")}

	reason, px, hit := touchReason("buy", &sl, &tp, spanning)
	if !hit || reason != "sl" {
		t.Errorf("a bar spanning both levels must resolve as sl, got %q (hit=%v)", reason, hit)
	}
	if !px.Equal(sl) {
		t.Errorf("exit price must be the stop, got %s", px)
	}

	// The mirror case for a short.
	slS, tpS := dec("110"), dec("95")
	if reason, _, _ := touchReason("sell", &slS, &tpS, spanning); reason != "sl" {
		t.Errorf("short: a spanning bar must resolve as sl, got %q", reason)
	}
}

func TestTouchReason_ResolvesEachLevelOnItsOwn(t *testing.T) {
	sl, tp := dec("95"), dec("110")
	if r, _, _ := touchReason("buy", &sl, &tp, domain.Candle{High: dec("112"), Low: dec("99")}); r != "tp" {
		t.Errorf("a bar reaching only the target must be tp, got %q", r)
	}
	if r, _, _ := touchReason("buy", &sl, &tp, domain.Candle{High: dec("101"), Low: dec("94")}); r != "sl" {
		t.Errorf("a bar reaching only the stop must be sl, got %q", r)
	}
	if _, _, hit := touchReason("buy", &sl, &tp, domain.Candle{High: dec("101"), Low: dec("99")}); hit {
		t.Error("a bar touching neither level must not close the position")
	}
}

// The strategy profile must reflect what THIS RUN has recorded, growing as the simulation trades.
//
// That is what makes a forward simulation coherent where rebuilding historical rows is not: the
// simulation always knows its own books. If the profile stayed empty, the model would be trained to
// ignore a strategy's track record precisely because it never saw one vary.
func TestRun_StrategyRecordAccumulatesDuringTheRun(t *testing.T) {
	_, sink, r := fixture([]string{"range_breakout_v2"})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	op := opens(sink.Samples)
	if len(op) < 3 {
		t.Skipf("need several open decisions, got %d", len(op))
	}
	first := op[0].Observation.Signal.TradeCount
	last := op[len(op)-1].Observation.Signal.TradeCount
	if first != 0 {
		t.Errorf("the first decision must see no track record, got %d trades", first)
	}
	if last <= first {
		t.Errorf("the record must grow during the run: first=%d last=%d", first, last)
	}
}

// One position per (strategy, instrument) — the live rule since 2026-09-14. A strategy stacking a
// second position before its first resolves would record one setup as several independent trials,
// corrupting exactly the per-strategy statistics the profile block feeds back to the model.
func TestRun_OneOpenPositionPerStrategy(t *testing.T) {
	_, sink, r := fixture([]string{"range_breakout_v2"})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var prevClose time.Time
	for i, s := range opens(sink.Samples) {
		if i > 0 && s.OpenedAt.Before(prevClose) {
			t.Fatalf("sample %d opened at %s, before the previous trade closed at %s",
				i, s.OpenedAt, prevClose)
		}
		prevClose = s.ClosedAt
	}
}

// No lookahead on the market-wide block: a decision made in January must see BTC as it was in
// January, not at the end of the run. This would be the most damaging leak available — it would
// tell the policy the future of the whole market.
func TestRun_BTCContextDoesNotLookAhead(t *testing.T) {
	src, sink, r := fixture([]string{"range_breakout_v2"})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.Samples) == 0 {
		t.Skip("no samples")
	}
	btc := src.series["BTC/5m"]
	finalBTC := btc[len(btc)-1].Close

	s := sink.Samples[0]
	if s.Observation.BTC.Close.Equal(finalBTC) {
		t.Error("the first decision sees BTC's FINAL close — the run is leaking the future of the " +
			"whole market into every earlier decision")
	}
}

// A run with no BTC history must fail loudly rather than emit a zeroed reference block, which would
// read as "BTC is perfectly flat and uncorrelated" — a specific false claim about the market.
func TestRun_RefusesWithoutBTCHistory(t *testing.T) {
	src := &fakeSource{series: map[string][]domain.Candle{"SOL/5m": trendingSeries(300, 150)}}
	r := testRunner(src, &MemorySink{}, []string{"range_breakout_v2"})

	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("expected a run with no BTC history to fail, got none")
	}
}

// Every opened trade must carry a stop. §16.9 records a real position opened with none — unbounded
// downside — because four independent gaps lined up; a dataset containing such trades would teach
// the policy that they are normal.
//
// Asserted against Terminal.PositionState.RiskPct, not Observation.Signal.SLPx: the signal is now
// (2026-09-15) the strategy's raw, unclamped proposal, exactly matching what production shows the
// model (papertrade.go retains resolved.SLPx/TPPx before the model is even asked, then clamps
// separately onto the ORDER) — a strategy with no structural stop of its own (§16.8: stoch_cross and
// others) legitimately has no Signal.SLPx, in production and here alike. RiskPct is computed from
// the position's actual entry-to-stop distance (backtest.go's riskPct, mirroring usecase.riskPct),
// which openPosition's own errNoStop check already guarantees is never nil — this is the invariant
// that must hold, not that the raw signal happened to propose one.
func TestRun_NoSampleWithoutAStop(t *testing.T) {
	_, sink, r := fixture(nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, s := range opens(sink.Samples) {
		if !s.Terminal.PositionState.RiskPct.IsPositive() {
			t.Fatalf("sample %d was opened with no stop", i)
		}
	}
}

// The open decision's Signal must carry the strategy's OWN, unclamped proposal — never the level
// the order was actually clamped to.
//
// Found 2026-09-15: buildObservation used to build Signal from `lv`, the clamps output, so a
// dataset built from a strategy proposing a wide stop would show the model a level §19.2/§45's
// clamps had already tightened — a real train/serve skew, since production
// (usecase.PaperTrader.evaluateStrategies, papertrade.go) always retains resolved.SLPx/TPPx BEFORE
// the model is even asked, and clamps only bear on the order placed afterwards. This test forces a
// real divergence: a 20% stop at 10x leverage is far outside both MaxSLDistPct (5%) and MaxLossPct
// (15%/10x = 1.5%), so the clamped order and the raw signal cannot coincide by chance.
func TestOpenObservation_SignalCarriesTheUnclampedProposal(t *testing.T) {
	_, _, r := fixture(nil)
	window := trendingSeries(300, 150)
	btcWindow := trendingSeries(300, 64000)
	c := window[len(window)-1]
	price := c.Close

	sig := strategy.Signal{Side: strategy.Buy, SLPct: dec("0.20"), TPPct: dec("0.40")}
	pos, err := r.openPosition("SOL", "5m", "test_kind", sig, window, btcWindow, len(window)-1, c)
	if err != nil {
		t.Fatalf("openPosition: %v", err)
	}

	resolved := sig.ResolveLevels(price)
	got := pos.openObs.Signal
	if got == nil {
		t.Fatal("open observation carries no signal")
	}
	if !got.SLPx.Equal(resolved.SLPx) {
		t.Errorf("Signal.SLPx = %s, want the raw proposal %s (unclamped)", got.SLPx, resolved.SLPx)
	}
	if !got.TPPx.Equal(resolved.TPPx) {
		t.Errorf("Signal.TPPx = %s, want the raw proposal %s (unclamped)", got.TPPx, resolved.TPPx)
	}

	// The clamp must still have actually bound the ORDER — proving this test exercises a real
	// divergence, not a coincidence where clamping happened to be a no-op.
	if pos.slPx == nil {
		t.Fatal("the order itself has no stop")
	}
	if pos.slPx.Equal(resolved.SLPx) {
		t.Fatal("the fixture's clamps did not actually tighten this stop — test proves nothing")
	}
}

// The account must carry across trades, so drawdown means something. Resetting per trade — or per
// instrument — would teach the policy that losses are wiped clean, the lesson §15.6 removed when it
// replaced per-token accounts with one shared pool.
func TestRun_AccountCarriesAcrossTrades(t *testing.T) {
	_, sink, r := fixture([]string{"range_breakout_v2", "macd_momentum"})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.Samples) < 2 {
		t.Skip("need at least two samples")
	}
	seen := map[string]bool{}
	for _, s := range sink.Samples {
		seen[s.Observation.AccountEquityUSD.String()] = true
	}
	if len(seen) == 1 {
		t.Error("every decision saw the same account equity — the simulated balance is not moving " +
			"with realized outcomes, so drawdown can never be learned")
	}
}

// Each trade needs its own order id, and the terminal call must carry the SAME one.
//
// The learner pairs a decision with the outcome that lands hours later by order id (§15.11) —
// pairing by arrival order would let a winning trade's reward train a losing trade's decision. A
// dataset whose samples share an id, or carry none, breaks that pairing silently; the live
// validator refuses a terminal call without one for the same reason, and that is what caught this.
func TestRun_EachTradeHasItsOwnOrderID(t *testing.T) {
	_, sink, r := fixture(nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.Samples) < 2 {
		t.Skip("need at least two samples")
	}

	// A trade now emits several samples — one open decision plus an `update` for each cadence tick
	// while it was held — and they SHARE an id by design: they are the same position, and the id is
	// what pairs each of them with the same outcome. What must stay unique is one OPEN DECISION per
	// id; two of those sharing one would mean two trades' rewards training against each other.
	seen := map[int64]bool{}
	for i, s := range sink.Samples {
		id := s.Observation.OrderID
		if id == 0 {
			t.Fatalf("sample %d has no order id; its reward could never be paired with its decision", i)
		}
		isOpen := s.Observation.Category == domain.CategoryBuy || s.Observation.Category == domain.CategorySell
		if isOpen {
			if seen[id] {
				t.Fatalf("sample %d reuses order id %d on a second OPEN decision — one trade's "+
					"reward would train another's", i, id)
			}
			seen[id] = true
		}

		if s.Terminal.OrderID != id {
			t.Fatalf("sample %d's terminal call carries id %d, the decision %d", i, s.Terminal.OrderID, id)
		}
	}
}

// A drained account must be reset and keep trading, exactly as paper trading does (§15.7).
//
// The first version of this package refused to reset, reasoning that it would teach the policy
// losses are wiped clean. That reasoning was wrong in a way only real data showed: across 26,593
// trades on the server's real history the account reached $0.000007 after 5,663 of them, and the
// remaining 21,000 samples were opened at sizes no real account would ever take. Production would
// have reset and carried on at a normal size, so those samples describe decisions it never makes —
// precisely the train/serve skew this rewrite exists to remove, and the observation's own equity
// ratio would have sat near zero throughout, a distribution the live model never sees.
//
// Driven directly rather than through a full run: whether a given fixture happens to drain is a
// property of the fixture, and a test that depends on it would pass or fail for reasons unrelated
// to the behaviour being asserted.
func TestOpenPosition_ResetsADrainedAccount(t *testing.T) {
	src, _, r := fixture([]string{"range_breakout_v2"})
	r.account = dec("0.004") // drained: below minTradableUSD
	r.peak = dec("40")
	r.records = map[string]*stratRecord{}
	r.result = Result{ByReason: map[string]int{}, Skipped: map[string]int{}}

	window := src.series["SOL/5m"]
	btc := src.series["BTC/5m"]
	sig := strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}

	p, err := r.openPosition("SOL", "5m", "range_breakout_v2", sig, window, btc, len(window)-1, window[len(window)-1])
	if err != nil {
		t.Fatalf("a drained account must reset and open, not refuse: %v", err)
	}
	if !r.account.Equal(r.Cfg.InitialUSD) {
		t.Errorf("the account must be topped back up to %s, got %s", r.Cfg.InitialUSD, r.account)
	}
	// The high-water mark resets with it, or every subsequent trade would be charged a drawdown
	// penalty measured against a peak the reset account can never reach again.
	if !r.peak.Equal(r.Cfg.InitialUSD) {
		t.Errorf("the peak must reset with the balance, got %s", r.peak)
	}
	if r.result.Resets != 1 {
		t.Errorf("the reset must be counted — an account draining repeatedly IS the finding (§15.7), got %d", r.result.Resets)
	}
	if p.size.LessThan(dec("0.1")) {
		t.Errorf("the position must be sized against the restored balance, got %s", p.size)
	}
}

// A healthy account is NOT reset. Guards the obvious mistake of resetting unconditionally, which
// would erase every drawdown before the penalty could ever charge for one.
func TestOpenPosition_DoesNotResetAHealthyAccount(t *testing.T) {
	src, _, r := fixture([]string{"range_breakout_v2"})
	r.account = dec("31")
	r.peak = dec("40")
	r.records = map[string]*stratRecord{}
	r.result = Result{ByReason: map[string]int{}, Skipped: map[string]int{}}

	window := src.series["SOL/5m"]
	btc := src.series["BTC/5m"]
	sig := strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}

	if _, err := r.openPosition("SOL", "5m", "range_breakout_v2", sig, window, btc, len(window)-1, window[len(window)-1]); err != nil {
		t.Fatalf("openPosition: %v", err)
	}
	if !r.account.Equal(dec("31")) {
		t.Errorf("a healthy account must be left alone, got %s", r.account)
	}
	if !r.peak.Equal(dec("40")) {
		t.Errorf("the peak must survive a drawdown, or the penalty can never charge for one; got %s", r.peak)
	}
	if r.result.Resets != 0 {
		t.Errorf("no reset should be counted, got %d", r.result.Resets)
	}
}

// One position must never commit the whole account, even with few slots configured.
//
// Measured on the test fixture without this cap: a $2 account with one slot at 10x compounded to
// $124 MILLION, an outcome no live configuration can produce because §15.6's max_position_pct is
// what prevents it. A dataset built that way would teach the policy that unbounded compounding is
// available to it.
func TestOpenPosition_CapsOnePositionAsAFractionOfEquity(t *testing.T) {
	src, _, r := fixture([]string{"range_breakout_v2"})
	r.account = dec("40")
	r.peak = dec("40")
	r.Cfg.PositionSlots = 1
	r.Cfg.MaxPositionPct = dec("0.25")
	r.records = map[string]*stratRecord{}
	r.result = Result{ByReason: map[string]int{}, Skipped: map[string]int{}}

	window := src.series["SOL/5m"]
	btc := src.series["BTC/5m"]
	sig := strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}

	p, err := r.openPosition("SOL", "5m", "range_breakout_v2", sig, window, btc, len(window)-1, window[len(window)-1])
	if err != nil {
		t.Fatalf("openPosition: %v", err)
	}
	if p.size.GreaterThan(dec("10")) {
		t.Errorf("one slot must still be capped at 25%% of a $40 account ($10), got %s", p.size)
	}
}

// Significance must be computed AFTER the per-kind derived fields it reads.
//
// It was not: SignificanceVsBaseline ran before the loop that fills PnLPerTrade, so every gap was
// computed from zeros and the whole table reported gap=0.0000, t=0.00. That is worse than a missing
// feature — it looks like a finished measurement saying "no strategy differs from random", which
// was also the conclusion being investigated, so it would have confirmed itself.
func TestRun_SignificanceIsComputedFromFilledStats(t *testing.T) {
	src := &fakeSource{series: map[string][]domain.Candle{
		"SOL/5m": trendingSeries(600, 150),
		"BTC/5m": trendingSeries(600, 64000),
	}}
	r := testRunner(src, &MemorySink{}, []string{"coin_flip", "macd_momentum", "vwap_reversion"})

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Significance) == 0 {
		t.Fatal("no significance reported despite the baseline being in the run")
	}

	nonZero := 0
	for _, s := range res.Significance {
		if s.Gap != 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Error("every gap is exactly 0.0000 — significance is reading PnLPerTrade before it is " +
			"filled, which reports a finished-looking table containing no measurement")
	}
}

// opens returns only the open-decision samples. The sink now also carries `update` samples, which
// deliberately have no Signal (§15.12: `present` is what tells the model no strategy spoke), so a
// test reading Samples[0].Signal would dereference nil and, worse, assert about the wrong call.
func opens(samples []Sample) []Sample {
	var out []Sample
	for _, s := range samples {
		if s.Observation.Category == domain.CategoryBuy || s.Observation.Category == domain.CategorySell {
			out = append(out, s)
		}
	}
	return out
}

// The dataset must contain `update` samples, not only open decisions and their outcomes.
//
// Without them the ten inputs of the position block — position_open, age, unrealized PnL, the two
// extremes, distance to each level — were zero on every one of 76,305 samples, while production
// fills them on every update call. That is the train/serve skew docs/RL_V8_PLAN.md exists to
// remove, and it was invisible until someone printed a vector and counted the zeros.
func TestRun_EmitsUpdateSamplesWithALivePositionBlock(t *testing.T) {
	_, sink, r := fixture(nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var updates int
	for i, s := range sink.Samples {
		if s.Observation.Category != domain.CategoryUpdate {
			continue
		}
		updates++
		ps := s.Observation.PositionState
		if !ps.PositionOpen {
			t.Fatalf("update %d reports no open position", i)
		}
		if ps.AgeSeconds <= 0 {
			t.Fatalf("update %d has age %d — the position block is still dead", i, ps.AgeSeconds)
		}
		if ps.SizeUSD.IsZero() || ps.Leverage.IsZero() {
			t.Fatalf("update %d carries no size/leverage", i)
		}
		if s.Observation.OrderID == 0 {
			t.Fatalf("update %d has no order id; its reward could not be paired", i)
		}
	}
	if updates == 0 {
		t.Fatal("no update samples at all — the position block can never be exercised")
	}
}

// An update carries the CARRIED signal — the last one this (instrument, bar) produced — exactly as
// lifecycle.go does via conductor.CarriedSignal. A 1H opinion stays meaningful for the whole hour,
// and dropping it the moment its candle closed would hide it from every update in between.
//
// This test exists because the first version of the backtest's update path sent nil on every
// cadence tick, reasoning that a carried signal is stale. Production does the opposite, so that
// produced a dataset where most updates had present=0 against a live path that sends present=1 —
// the train/serve skew the whole plan exists to remove, reintroduced by the code meant to close it.
func TestRun_UpdateCarriesTheSignalForwardLikeProduction(t *testing.T) {
	_, sink, r := fixture(nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var updates, withSignal int
	for i, s := range sink.Samples {
		if s.Observation.Category != domain.CategoryUpdate {
			continue
		}
		updates++
		if s.Observation.Signal == nil {
			continue
		}
		withSignal++
		// A carried signal must still be coherent: side set and levels on the right sides of entry.
		// Carrying forward a malformed one would be worse than carrying none.
		sg := s.Observation.Signal
		if sg.Side == "" {
			t.Fatalf("update %d carries a signal with no side", i)
		}
		if !sg.EntryPx.IsPositive() {
			t.Fatalf("update %d carries a signal with no entry price", i)
		}
	}
	if updates == 0 {
		t.Fatal("no update samples at all")
	}
	// Every update should carry one: by the time a position is open, its own signal has fired and
	// been retained. Nil is reserved for a bar where no strategy has ever spoken, which cannot
	// happen while a position from that bar is being held.
	if withSignal != updates {
		t.Errorf("%d of %d updates carry no signal — carry-forward is not reaching them",
			updates-withSignal, updates)
	}
}

// Age must come from the candle's own timestamp, never from a bar count. A timeframe with a gap —
// an exchange outage leaves missing candles — ages the position by the real elapsed time, and
// counting bars would under-report it by exactly the length of the gap.
func TestRun_UpdateAgeComesFromTimestampsNotBarCount(t *testing.T) {
	_, sink, r := fixture(nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, s := range sink.Samples {
		if s.Observation.Category != domain.CategoryUpdate {
			continue
		}
		want := int64(s.ClosedAt.Sub(s.OpenedAt).Seconds())
		got := s.Observation.PositionState.AgeSeconds
		if got <= 0 || got > want {
			t.Fatalf("update %d: age %ds is outside the trade's own span of %ds", i, got, want)
		}
	}
}
