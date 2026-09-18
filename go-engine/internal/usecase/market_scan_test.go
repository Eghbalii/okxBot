package usecase

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// okxSuffixes/mexcSuffixes mirror what config.example.yaml ships, so these tests exercise the real
// patterns rather than ones invented to pass.
var (
	okxSuffixes  = []string{"-USD_UM_XPERP-", "-USDT-SWAP", "-USD-SWAP"}
	mexcSuffixes = []string{"_USDT"}
)

// TestSymbolFromInstID_RejectsDatedFutures is the safety-critical case.
//
// OKX's instType=FUTURES carries 179 X-Perp perpetuals alongside 28 DATED contracts — live-checked
// 2026-09-13, eleven for BTC alone. Admitting a dated contract to a perpetual-futures bot would put
// real orders on an instrument that expires, and the two ids differ by very little
// ("BTC-USD_UM-260925" is one underscore-delimited token short of the perpetual's own pattern).
//
// Every id below is a real one from that response, and each is rejected because it contains none of
// the configured suffixes — the suffix patterns themselves are the defence here, which is what makes
// this test worth having: it pins that those specific patterns discriminate correctly, so widening
// one later (a plausible change, since OKX rolls these ids) fails here rather than in production.
func TestSymbolFromInstID_RejectsDatedFutures(t *testing.T) {
	for _, id := range []string{
		"BTC-USD-270924",    // dated, quarterly
		"BTC-USD_UM-260925", // dated, USDC-margined
		"BTC-USD-261225",
		"ETH-USD_UM-270326",
		"SOL-USD_UM-261030",
		"XAU-USD_UM-261030",
	} {
		if sym, ok := SymbolFromInstID(id, okxSuffixes); ok {
			t.Errorf("%s: admitted as symbol %q — a dated future must never be treated as a perpetual", id, sym)
		}
	}
}

// A suffix pattern loose enough to match mid-id yields a "symbol" carrying a separator, which is
// never a real token name. Not a live case — the shipped patterns never produce it — but the cheap
// invariant that keeps a future widening of those patterns from silently admitting a dated contract
// under a mangled symbol.
func TestSymbolFromInstID_RejectsASymbolCarryingASeparator(t *testing.T) {
	// "-SWAP" is deliberately too loose: it matches inside "BTC-USDT-SWAP" after the quote currency.
	if sym, ok := SymbolFromInstID("BTC-USDT-SWAP", []string{"-SWAP"}); ok {
		t.Errorf("admitted %q from a too-loose pattern — a symbol may not contain a separator", sym)
	}
}

func TestSymbolFromInstID_AcceptsPerpetuals(t *testing.T) {
	cases := []struct {
		id, want string
		suffixes []string
	}{
		{"BTC-USD_UM_XPERP-310404", "BTC", okxSuffixes},
		{"PEPE-USD_UM_XPERP-310822", "PEPE", okxSuffixes},
		{"BTC-USDT-SWAP", "BTC", okxSuffixes},
		{"EDGE-USDT-SWAP", "EDGE", okxSuffixes},
		{"BTC_USDT", "BTC", mexcSuffixes},
		{"1000PEPE_USDT", "1000PEPE", mexcSuffixes},
	}
	for _, c := range cases {
		got, ok := SymbolFromInstID(c.id, c.suffixes)
		if !ok {
			t.Errorf("%s: rejected, want symbol %q", c.id, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.id, got, c.want)
		}
	}
}

// An exchange with no configured suffixes must scan nothing rather than guess at its naming — a
// wrong guess silently admits spot pairs or dated futures.
func TestSymbolFromInstID_NoSuffixesAdmitsNothing(t *testing.T) {
	if _, ok := SymbolFromInstID("BTC-USDT-SWAP", nil); ok {
		t.Error("an exchange with no configured suffixes must admit nothing")
	}
}

func mt(instID string, last, open, high, low, vol float64) domain.MarketTicker {
	f := decimal.NewFromFloat
	m := domain.MarketTicker{
		InstID: instID, Last: f(last), Open24h: f(open),
		High24h: f(high), Low24h: f(low), Vol24hUSD: f(vol),
	}
	if open != 0 {
		m.Change24hPct = m.Last.Sub(m.Open24h).Div(m.Open24h).Mul(decimal.NewFromInt(100))
	}
	return m
}

// TestScoreToken_VolumeDominates pins the weighting decision: liquidity is what gates whether this
// bot can trade a token at all (§33.2 found the execution venue's thinness, not the signal, was the
// binding constraint), so a deep market beats a thin one that happens to have moved more.
func TestScoreToken_VolumeDominates(t *testing.T) {
	maxVol := decimal.NewFromInt(1_000_000_000)
	deep := mt("BTC-USDT-SWAP", 77000, 76500, 77500, 76400, 1_000_000_000) // +0.65%, deep
	thin := mt("JUNK-USDT-SWAP", 0.001, 0.0005, 0.0012, 0.0004, 2_000_000) // +100%, thin

	ds, ts := ScoreToken(deep, maxVol), ScoreToken(thin, maxVol)
	if !ds.GreaterThan(ts) {
		t.Errorf("deep market scored %s, thin market %s — volume must dominate", ds, ts)
	}
}

// A big move in EITHER direction is equally interesting to a bot that trades both sides (§9).
// Signing the change would rank the market by direction, which is a prediction the scan must not make.
func TestScoreToken_DirectionAgnostic(t *testing.T) {
	maxVol := decimal.NewFromInt(100_000_000)
	up := mt("A-USDT-SWAP", 110, 100, 111, 99, 50_000_000)
	down := mt("B-USDT-SWAP", 90, 100, 101, 89, 50_000_000)
	// Same 10% move, same range, same volume — the scores must match.
	if u, d := ScoreToken(up, maxVol), ScoreToken(down, maxVol); !u.Equal(d) {
		t.Errorf("up scored %s but down scored %s — the score must be direction-agnostic", u, d)
	}
}

// Range is the component that survives a token moving hard both ways and coming back — exactly the
// day a mean-reversion strategy has the most to work with, and one a change-only ranking is blind to.
func TestRangePct_SurvivesARoundTrip(t *testing.T) {
	roundTrip := mt("A-USDT-SWAP", 100, 100, 115, 95, 10_000_000) // 0% change, 20% range
	if !roundTrip.Change24hPct.IsZero() {
		t.Fatalf("fixture is wrong: change = %s", roundTrip.Change24hPct)
	}
	if got := RangePct(roundTrip); !got.Equal(decimal.NewFromInt(20)) {
		t.Errorf("RangePct = %s, want 20", got)
	}
}

type stubTickers struct {
	toks []domain.MarketTicker
	err  error
}

func (s stubTickers) GetAllTickers(string) ([]domain.MarketTicker, error) { return s.toks, s.err }

func scannerFor(repo port.Repository, client allTickerFetcher, topN int) *MarketScanner {
	return &MarketScanner{
		Repo: repo,
		Exchanges: []ExchangeSource{{
			// TradesLive: true matches production's own cmd/api wiring for "okx" — this helper
			// always builds an okx source, which genuinely is the exchange paper-trader loads.
			Name: "okx", Client: client, InstType: "FUTURES", QuoteSuffixes: okxSuffixes, TradesLive: true,
		}},
		TopN: topN,
	}
}

// TestScan_AdmitsWithRealDisabled pins the operator's instruction directly (2026-09-13): a
// discovered token joins data collection and paper trading immediately, and is added to real mode
// DISABLED for a person to turn on. Discovery must never be the same decision as real-money exposure.
func TestScan_AdmitsWithRealDisabled(t *testing.T) {
	repo := newFakeRepository()
	sc := scannerFor(repo, stubTickers{toks: []domain.MarketTicker{
		mt("BTC-USD_UM_XPERP-310404", 77000, 76000, 78000, 75500, 69_000_000),
	}}, 10)

	if res := sc.Scan(context.Background()); res[0].Err != nil || res[0].Admitted != 1 {
		t.Fatalf("scan: %+v", res[0])
	}

	roster, err := repo.ListInstruments(context.Background(), port.InstrumentFilter{})
	if err != nil || len(roster) != 1 {
		t.Fatalf("roster = %d rows, err %v", len(roster), err)
	}
	got := roster[0]
	if !got.EnabledIngest || !got.EnabledPaper {
		t.Errorf("ingest=%v paper=%v — a discovered token must start collecting data and paper trading", got.EnabledIngest, got.EnabledPaper)
	}
	if got.EnabledReal {
		t.Error("enabled_real is true — a scan must never enable a token for real money")
	}
	if got.Source != "scan" || got.Symbol != "BTC" || got.ExecInstID != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("unexpected row: %+v", got)
	}
}

// A scan re-finding a token an operator has since DISABLED must leave it disabled. This is migration
// 000030's bug in a new place: a service overruling a person's choice because it cannot tell that
// choice from its own earlier one.
func TestScan_DoesNotResurrectADisabledToken(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	client := stubTickers{toks: []domain.MarketTicker{
		mt("BTC-USD_UM_XPERP-310404", 77000, 76000, 78000, 75500, 69_000_000),
	}}
	sc := scannerFor(repo, client, 10)
	sc.Scan(ctx)

	roster, _ := repo.ListInstruments(ctx, port.InstrumentFilter{})
	off := false
	if err := repo.SetInstrumentFlags(ctx, roster[0].ID, port.InstrumentPatch{EnabledPaper: &off, EnabledIngest: &off}); err != nil {
		t.Fatal(err)
	}

	sc.Scan(ctx) // the same token, found again

	roster, _ = repo.ListInstruments(ctx, port.InstrumentFilter{})
	if len(roster) != 1 {
		t.Fatalf("roster = %d rows, want the same single row refreshed", len(roster))
	}
	if roster[0].EnabledIngest || roster[0].EnabledPaper {
		t.Error("a re-found token was re-enabled — the operator's decision was overruled")
	}
}

// TopN bounds what is admitted: every admitted token costs a WS subscription, a candle window per
// timeframe, and a share of the shared account through dynamic sizing (§32.4), so admitting
// everything good would dilute every position toward nothing.
func TestScan_AdmitsOnlyTopN(t *testing.T) {
	var toks []domain.MarketTicker
	for _, s := range []string{"AAA", "BBB", "CCC", "DDD", "EEE"} {
		toks = append(toks, mt(s+"-USDT-SWAP", 10, 9, 11, 8, 50_000_000))
	}
	repo := newFakeRepository()
	sc := scannerFor(repo, stubTickers{toks: toks}, 2)

	res := sc.Scan(context.Background())
	if res[0].Candidates != 5 || res[0].Admitted != 2 {
		t.Errorf("candidates=%d admitted=%d, want 5 and 2", res[0].Candidates, res[0].Admitted)
	}
	// The full market is still stored for the panel even though only two were admitted — the ranked
	// view and the working set are different things.
	stored, _ := repo.ListMarketTokens(context.Background(), "okx", 0)
	if len(stored) != 5 {
		t.Errorf("stored %d market tokens, want all 5 scanned", len(stored))
	}
}

// A market under the volume floor is not a candidate at any score: it cannot absorb even this
// project's small positions without moving, so ranking it highly on a big percentage move would be
// actively misleading.
func TestScan_AppliesVolumeFloor(t *testing.T) {
	repo := newFakeRepository()
	sc := scannerFor(repo, stubTickers{toks: []domain.MarketTicker{
		mt("DUST-USDT-SWAP", 1, 0.5, 1.2, 0.4, 500), // +100% on $500 of volume
	}}, 10)

	res := sc.Scan(context.Background())
	if res[0].Candidates != 0 || res[0].Admitted != 0 {
		t.Errorf("candidates=%d admitted=%d, want 0 — the volume floor was not applied", res[0].Candidates, res[0].Admitted)
	}
}

// One exchange failing must be reported as itself and must NOT clear its last known-good snapshot:
// an exchange returning nothing is a fault, and deleting the only view of its market on a fault is
// the opposite of useful.
func TestScan_ExchangeFailureKeepsPreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	good := stubTickers{toks: []domain.MarketTicker{mt("BTC-USDT-SWAP", 77000, 76000, 78000, 75000, 1_000_000_000)}}
	sc := scannerFor(repo, good, 10)
	sc.Scan(ctx)

	before, _ := repo.ListMarketTokens(ctx, "okx", 0)
	if len(before) != 1 {
		t.Fatalf("setup: %d tokens", len(before))
	}

	sc.Exchanges[0].Client = stubTickers{err: context.DeadlineExceeded}
	res := sc.Scan(ctx)
	if res[0].Err == nil {
		t.Error("a failing exchange must report its own error")
	}
	after, _ := repo.ListMarketTokens(ctx, "okx", 0)
	if len(after) != 1 {
		t.Errorf("snapshot has %d tokens after a failed scan, want the previous 1 retained", len(after))
	}
}

// A genuinely new token tops up the paper account by PerTokenCapUSD (2026-09-17 request): the
// account cap now follows the live enabled-token count rather than staying fixed while the roster
// grows underneath it, which had fragmented every position toward a few cents as discovery kept
// adding tokens the fixed $40 was never resized for.
func TestScan_ToppedUpAccountForANewlyAdmittedToken(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40)); err != nil {
		t.Fatal(err)
	}

	sc := scannerFor(repo, stubTickers{toks: []domain.MarketTicker{
		mt("BTC-USD_UM_XPERP-310404", 77000, 76000, 78000, 75500, 69_000_000),
	}}, 10)
	sc.PerTokenCapUSD = decimal.NewFromInt(4)

	if res := sc.Scan(ctx); res[0].Err != nil || res[0].NewlyAdmitted != 1 {
		t.Fatalf("scan: %+v", res[0])
	}

	ae, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40))
	if err != nil {
		t.Fatal(err)
	}
	if !ae.EquityUSD.Equal(decimal.NewFromInt(44)) {
		t.Errorf("equity = %s, want 44 (40 + one $4 top-up)", ae.EquityUSD)
	}
	if !ae.AccountBalanceUSD.Equal(decimal.NewFromInt(44)) {
		t.Errorf("balance = %s, want 44 — a top-up must move both figures together", ae.AccountBalanceUSD)
	}
}

// A token the scan re-finds (already on the roster, only its market snapshot refreshed) must NOT
// top up the account a second time — that would inflate the account for a roster that did not grow,
// exactly what topUpForNewTokens's wasNew check exists to prevent.
func TestScan_DoesNotTopUpForARefreshedToken(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40)); err != nil {
		t.Fatal(err)
	}

	toks := []domain.MarketTicker{mt("BTC-USD_UM_XPERP-310404", 77000, 76000, 78000, 75500, 69_000_000)}
	sc := scannerFor(repo, stubTickers{toks: toks}, 10)
	sc.PerTokenCapUSD = decimal.NewFromInt(4)

	sc.Scan(ctx) // first sighting: tops up once
	sc.Scan(ctx) // same token again: must not top up a second time

	ae, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40))
	if err != nil {
		t.Fatal(err)
	}
	if !ae.EquityUSD.Equal(decimal.NewFromInt(44)) {
		t.Errorf("equity = %s, want 44 — a re-scan of the same token must not top up again", ae.EquityUSD)
	}
}

// PerTokenCapUSD left at its zero value must disable the top-up entirely — a caller (or an
// exchange with no account concept) that never opted in must never see Scan touch money.
func TestScan_ZeroPerTokenCapDisablesTopUp(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40)); err != nil {
		t.Fatal(err)
	}

	sc := scannerFor(repo, stubTickers{toks: []domain.MarketTicker{
		mt("BTC-USD_UM_XPERP-310404", 77000, 76000, 78000, 75500, 69_000_000),
	}}, 10)
	// PerTokenCapUSD deliberately left at its zero value.

	sc.Scan(ctx)

	ae, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40))
	if err != nil {
		t.Fatal(err)
	}
	if !ae.EquityUSD.Equal(decimal.NewFromInt(40)) {
		t.Errorf("equity = %s, want unchanged at 40 — PerTokenCapUSD is zero, the top-up must be a no-op", ae.EquityUSD)
	}
}

// A newly-admitted token on an exchange with TradesLive=false must NOT top up the account (found
// in production, 2026-09-18): paper-trader's own roster load is hardcoded to "okx" only (no MEXC
// execution wiring exists yet), so a MEXC admission can never spend a share of the sizing budget
// the way an OKX one does. Before this fix, Scan topped up for every admission across every
// configured exchange regardless — the operator noticed the paper account's equity had grown well
// past what the OKX-only roster's own token count justified, traced to four MEXC-only admissions.
func TestScan_DoesNotTopUpForANonTradingExchange(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40)); err != nil {
		t.Fatal(err)
	}

	sc := &MarketScanner{
		Repo: repo,
		Exchanges: []ExchangeSource{{
			Name: "mexc", Client: stubTickers{toks: []domain.MarketTicker{
				mt("BTCUSDT", 77000, 76000, 78000, 75500, 69_000_000),
			}}, InstType: "FUTURES", QuoteSuffixes: []string{"USDT"},
			// TradesLive deliberately left false — the exact MEXC-shaped case this test pins.
		}},
		TopN:           10,
		PerTokenCapUSD: decimal.NewFromInt(4),
	}

	if res := sc.Scan(ctx); res[0].Err != nil || res[0].NewlyAdmitted != 1 {
		t.Fatalf("scan: %+v", res[0])
	}

	ae, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40))
	if err != nil {
		t.Fatal(err)
	}
	if !ae.EquityUSD.Equal(decimal.NewFromInt(40)) {
		t.Errorf("equity = %s, want unchanged at 40 — a non-tradeable exchange's admission must never top up the account", ae.EquityUSD)
	}
}

// The mixed case: one exchange trades live, one does not. Only the tradeable one's admission
// should count toward the top-up.
func TestScan_OnlyTradesLiveExchangeCountsTowardTopUp(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40)); err != nil {
		t.Fatal(err)
	}

	sc := &MarketScanner{
		Repo: repo,
		Exchanges: []ExchangeSource{
			{
				Name: "okx", Client: stubTickers{toks: []domain.MarketTicker{
					mt("BTC-USD_UM_XPERP-310404", 77000, 76000, 78000, 75500, 69_000_000),
				}}, InstType: "FUTURES", QuoteSuffixes: okxSuffixes, TradesLive: true,
			},
			{
				Name: "mexc", Client: stubTickers{toks: []domain.MarketTicker{
					mt("ETHUSDT", 3000, 2900, 3100, 2850, 50_000_000),
				}}, InstType: "FUTURES", QuoteSuffixes: []string{"USDT"},
			},
		},
		TopN:           10,
		PerTokenCapUSD: decimal.NewFromInt(4),
	}

	sc.Scan(ctx)

	ae, err := repo.GetAccountEquity(ctx, "paper", decimal.NewFromInt(40))
	if err != nil {
		t.Fatal(err)
	}
	// Exactly ONE top-up (the OKX admission), not two.
	if !ae.EquityUSD.Equal(decimal.NewFromInt(44)) {
		t.Errorf("equity = %s, want 44 (one $4 top-up for the OKX admission only, MEXC's must not count)", ae.EquityUSD)
	}
}
