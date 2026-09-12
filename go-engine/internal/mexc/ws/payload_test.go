package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/shopspring/decimal"
)

func bar(tsUnix int64, close float64) domain.Candle {
	return domain.Candle{
		Timestamp: time.Unix(tsUnix, 0).UTC(),
		Open:      decimal.NewFromFloat(close),
		High:      decimal.NewFromFloat(close),
		Low:       decimal.NewFromFloat(close),
		Close:     decimal.NewFromFloat(close),
	}
}

// TestKlineFinalizer_EmitsEachBarExactlyOnce is the core contract. MEXC re-pushes the forming bar
// continuously with no closed-flag, so the finalizer must emit a bar when the NEXT one opens —
// once, never twice. Emitting twice would double-write candles and re-trigger strategy evaluation
// on a bar already traded.
func TestKlineFinalizer_EmitsEachBarExactlyOnce(t *testing.T) {
	f := NewKlineFinalizer()
	const sym, iv = "BTC_USDT", "Min5"

	// Bar A pushed four times as it forms — no finalization yet, it is still open.
	for i, px := range []float64{100, 101, 102, 103} {
		if _, ok := f.Observe(sym, iv, bar(1000, px)); ok {
			t.Fatalf("push %d finalized a bar that is still forming", i)
		}
	}

	// Bar B opens: bar A is now complete, and must be emitted with its LAST observed state.
	got, ok := f.Observe(sym, iv, bar(1300, 104))
	if !ok {
		t.Fatal("a newer bar opened but the previous one was not finalized")
	}
	if got.Timestamp.Unix() != 1000 {
		t.Errorf("finalized bar ts = %d, want 1000 (the CLOSED bar, not the new one)", got.Timestamp.Unix())
	}
	if !got.Close.Equal(decimal.NewFromFloat(103)) {
		t.Errorf("finalized close = %s, want 103 (bar A's last observed state)", got.Close)
	}

	// Bar B still forming: no further emission for A.
	if _, ok := f.Observe(sym, iv, bar(1300, 105)); ok {
		t.Fatal("bar A was finalized a second time")
	}
}

// TestKlineFinalizer_FirstPushNeverFinalizes — on connect, the first push is for a bar already in
// progress. Emitting it would write a candle whose high/low cover only the fraction of the period
// this process was connected for.
func TestKlineFinalizer_FirstPushNeverFinalizes(t *testing.T) {
	f := NewKlineFinalizer()
	if _, ok := f.Observe("BTC_USDT", "Min5", bar(1000, 100)); ok {
		t.Fatal("the very first push finalized something — there was no prior bar to close")
	}
}

// TestKlineFinalizer_StreamsAreIndependent guards the bug a single shared "last bar" would cause:
// this process subscribes to several instruments AND several timeframes on one connection
// (CLAUDE.md §9), so a 5m push must never finalize a 1H candle, and BTC must never finalize ETH's.
func TestKlineFinalizer_StreamsAreIndependent(t *testing.T) {
	f := NewKlineFinalizer()

	// Bar times are deliberately INTERLEAVED across streams, which is the real-world case: a 1H
	// bar and a 5m bar have different open times, and different symbols update at different
	// moments. An earlier version of this test used the same timestamp for every stream, which
	// made a shared-key implementation behave identically to a correct one — it passed against the
	// bug it was written to catch.
	f.Observe("BTC_USDT", "Min5", bar(1000, 100))
	f.Observe("ETH_USDT", "Min5", bar(1200, 50)) // different bar time
	f.Observe("BTC_USDT", "Hour1", bar(900, 100))

	// A newer BTC 5m bar finalizes ONLY BTC's 5m, with BTC's own price.
	got, ok := f.Observe("BTC_USDT", "Min5", bar(1300, 101))
	if !ok {
		t.Fatal("BTC 5m did not finalize")
	}
	if !got.Close.Equal(decimal.NewFromFloat(100)) {
		t.Fatalf("BTC 5m finalized with close %s, want 100 — a shared key would emit another "+
			"stream's bar here", got.Close)
	}

	// ETH's 5m is still on bar 1200 and must NOT have been finalized by BTC's push. With a shared
	// key, BTC's ts=1300 push would have overwritten ETH's tracked bar, so this repeat of ETH's own
	// bar 1200 would look like an older bar and behave differently.
	if c, ok := f.Observe("ETH_USDT", "Min5", bar(1200, 51)); ok {
		t.Errorf("ETH 5m was finalized by a BTC push (emitted close=%s)", c.Close)
	}
	// ETH's own next bar must finalize ETH's bar 1200 with ETH's price, not BTC's.
	ethGot, ok := f.Observe("ETH_USDT", "Min5", bar(1500, 52))
	if !ok {
		t.Fatal("ETH 5m did not finalize on its own next bar")
	}
	if !ethGot.Close.Equal(decimal.NewFromFloat(51)) {
		t.Errorf("ETH finalized with close %s, want 51 — streams are sharing state", ethGot.Close)
	}

	// And the 1H stream must still be tracking its own bar 900, unaffected by either 5m stream.
	hourGot, ok := f.Observe("BTC_USDT", "Hour1", bar(4500, 110))
	if !ok {
		t.Fatal("BTC 1H did not finalize on its own next bar")
	}
	if !hourGot.Close.Equal(decimal.NewFromFloat(100)) {
		t.Errorf("BTC 1H finalized with close %s, want 100 — a 5m push corrupted the 1H stream", hourGot.Close)
	}
}

// TestKlineFinalizer_IgnoresOutOfOrderPush — a stale push for an older bar must never finalize
// anything, since that bar was already emitted.
func TestKlineFinalizer_IgnoresOutOfOrderPush(t *testing.T) {
	f := NewKlineFinalizer()
	const sym, iv = "BTC_USDT", "Min5"
	f.Observe(sym, iv, bar(1000, 100))
	f.Observe(sym, iv, bar(1300, 101)) // finalizes bar 1000

	if _, ok := f.Observe(sym, iv, bar(1000, 99)); ok {
		t.Fatal("an out-of-order push for an older bar finalized a candle")
	}
}

// TestDecodeKline_ReadsLiveWireShape uses the exact bytes captured from the live socket, so the
// field names and the seconds-timestamp are asserted against reality rather than against my own
// reading of the docs.
func TestDecodeKline_ReadsLiveWireShape(t *testing.T) {
	raw := `{"symbol":"BTC_USDT","interval":"Min5","t":1789248000,"o":77135.2,"c":77135.1,` +
		`"h":77135.2,"l":77135.1,"a":1365315.89911,"q":177003,"ro":77135.1,"rc":77135.1}`
	c, err := DecodeKline(Message{Channel: "push.kline", Data: json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Timestamp.Unix() != 1789248000 {
		t.Errorf("ts = %d, want 1789248000", c.Timestamp.Unix())
	}
	// A seconds value misread as milliseconds lands ~56,000 years out; this pins the unit.
	if y := c.Timestamp.UTC().Year(); y < 2020 || y > 2100 {
		t.Errorf("timestamp year %d — seconds/milliseconds confusion", y)
	}
	if !c.Open.Equal(decimal.NewFromFloat(77135.2)) {
		t.Errorf("open = %s, want 77135.2", c.Open)
	}
	if !c.Close.Equal(decimal.NewFromFloat(77135.1)) {
		t.Errorf("close = %s, want 77135.1", c.Close)
	}
	if !c.Volume.Equal(decimal.NewFromInt(177003)) {
		t.Errorf("volume = %s, want 177003 (q, the contract volume)", c.Volume)
	}
}

// TestDecodeTicker_ReadsLiveWireShape — same, for the ticker channel.
func TestDecodeTicker_ReadsLiveWireShape(t *testing.T) {
	raw := `{"symbol":"BTC_USDT","lastPrice":77135.2,"riseFallRate":-0.0026,"fairPrice":77135.1,` +
		`"indexPrice":77172,"volume24":197080484,"lower24Price":76942.5,"high24Price":77477.4,` +
		`"timestamp":1789248117768,"bid1":77135.1,"ask1":77135.2}`
	tk, err := DecodeTicker(Message{Channel: "push.ticker", Data: json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tk.InstID != "BTC_USDT" {
		t.Errorf("InstID = %q", tk.InstID)
	}
	if !tk.Last.Equal(decimal.NewFromFloat(77135.2)) {
		t.Errorf("Last = %s, want 77135.2", tk.Last)
	}
	if tk.BidPx.GreaterThan(tk.AskPx) {
		t.Errorf("bid %s above ask %s — fields crossed", tk.BidPx, tk.AskPx)
	}
}

// TestMessage_AckIsNotMistakenForData guards the exact failure CLAUDE.md §14 records for OKX: an
// ack frame misclassified as a data push broke every decode, and only surfaced against the live
// exchange. MEXC's ack carries the literal string "success" as its data, which would decode into a
// caller's struct as empty values — a silent wrong answer rather than an error.
func TestMessage_AckIsNotMistakenForData(t *testing.T) {
	ack := Message{Channel: "rs.sub.ticker", Data: json.RawMessage(`"success"`)}
	if ack.isDataPush() {
		t.Error("subscribe ack classified as a data push")
	}
	if !ack.isAck() {
		t.Error("subscribe ack not recognised as an ack")
	}
	push := Message{Channel: "push.ticker", Data: json.RawMessage(`{"symbol":"BTC_USDT"}`)}
	if !push.isDataPush() {
		t.Error("data push not recognised")
	}
	if push.isAck() {
		t.Error("data push classified as an ack")
	}
}
