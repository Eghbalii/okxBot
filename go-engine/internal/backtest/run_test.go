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
			InstIDs:       []string{"SOL"},
			Bars:          []string{"5m"},
			Kinds:         kinds,
			InitialUSD:    dec("40"),
			MaxLeverage:   dec("10"),
			PositionSlots: 16,
			CandleWindow:  300,
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
	s := sink.Samples[0]
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
	if len(sink.Samples) < 3 {
		t.Skipf("need several samples, got %d", len(sink.Samples))
	}
	first := sink.Samples[0].Observation.Signal.TradeCount
	last := sink.Samples[len(sink.Samples)-1].Observation.Signal.TradeCount
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
	for i, s := range sink.Samples {
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
func TestRun_NoSampleWithoutAStop(t *testing.T) {
	_, sink, r := fixture(nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, s := range sink.Samples {
		if s.Observation.Signal == nil || !s.Observation.Signal.SLPx.IsPositive() {
			t.Fatalf("sample %d was opened with no stop", i)
		}
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

	seen := map[int64]bool{}
	for i, s := range sink.Samples {
		id := s.Observation.OrderID
		if id == 0 {
			t.Fatalf("sample %d has no order id; its reward could never be paired with its decision", i)
		}
		if seen[id] {
			t.Fatalf("sample %d reuses order id %d — one trade's reward would train another's decision", i, id)
		}
		seen[id] = true

		if s.Terminal.OrderID != id {
			t.Fatalf("sample %d's terminal call carries id %d, the decision %d", i, s.Terminal.OrderID, id)
		}
	}
}
