package usecase

import (
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

func TestDecodeTick_ParsesMatchingInstrument(t *testing.T) {
	data := []byte(`{"instId":"BTC-USDT-SWAP","last":"50000.5"}`)
	price, ok, err := decodeTick(data, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for a matching instrument")
	}
	if !price.Equal(decimal.RequireFromString("50000.5")) {
		t.Errorf("unexpected price: %s", price)
	}
}

func TestDecodeTick_SkipsOtherInstrumentsWithoutError(t *testing.T) {
	data := []byte(`{"instId":"ETH-USDT-SWAP","last":"3000"}`)
	_, ok, err := decodeTick(data, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("unexpected error for a shared-topic message from another instrument: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a non-matching instrument")
	}
}

func TestDecodeTick_ErrorsOnMalformedJSON(t *testing.T) {
	_, _, err := decodeTick([]byte(`not json`), "BTC-USDT-SWAP")
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestDecodeTick_ErrorsOnUnparsablePrice(t *testing.T) {
	data := []byte(`{"instId":"BTC-USDT-SWAP","last":"not-a-number"}`)
	_, _, err := decodeTick(data, "BTC-USDT-SWAP")
	if err == nil {
		t.Fatal("expected an error for an unparsable price")
	}
}

func TestDecodeCandle_ParsesConfirmedCandle(t *testing.T) {
	// OKX candle array: [ts, o, h, l, c, vol, volCcy, volCcyQuote, confirm]
	data := []byte(`{"instId":"BTC-USDT-SWAP","bar":"1m","candle":["1700000000000","100","110","90","105","10","1000","1000","1"]}`)
	dc, ok, err := decodeCandle(data, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !dc.Confirmed {
		t.Error("expected Confirmed=true for confirm=1")
	}
	if !dc.Candle.Close.Equal(decimal.RequireFromString("105")) {
		t.Errorf("unexpected close: %s", dc.Candle.Close)
	}
}

func TestDecodeCandle_UnconfirmedStillFormingBar(t *testing.T) {
	data := []byte(`{"instId":"BTC-USDT-SWAP","bar":"1m","candle":["1700000000000","100","110","90","105","10","1000","1000","0"]}`)
	dc, ok, err := decodeCandle(data, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true (still a valid decode, just not confirmed)")
	}
	if dc.Confirmed {
		t.Error("expected Confirmed=false for confirm=0")
	}
}

func TestDecodeCandle_SkipsOtherInstrumentsWithoutError(t *testing.T) {
	data := []byte(`{"instId":"ETH-USDT-SWAP","bar":"1m","candle":["1700000000000","100","110","90","105","10","1000","1000","1"]}`)
	_, ok, err := decodeCandle(data, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("unexpected error for a shared-topic message from another instrument: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a non-matching instrument")
	}
}

func TestDecodeCandle_SkipsTooShortCandleArrayWithoutError(t *testing.T) {
	data := []byte(`{"instId":"BTC-USDT-SWAP","bar":"1m","candle":["1700000000000","100"]}`)
	_, ok, err := decodeCandle(data, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("unexpected error for a too-short candle array: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a too-short candle array")
	}
}

func TestDecodeCandle_ErrorsOnUnparsableField(t *testing.T) {
	data := []byte(`{"instId":"BTC-USDT-SWAP","bar":"1m","candle":["1700000000000","not-a-number","110","90","105","10"]}`)
	_, _, err := decodeCandle(data, "BTC-USDT-SWAP")
	if err == nil {
		t.Fatal("expected an error for an unparsable candle field")
	}
}

func TestDecisionBarFor_ExplicitOverridesDefault(t *testing.T) {
	if got := decisionBarFor("1H", []string{"5m", "15m"}); got != "1H" {
		t.Errorf("expected explicit bar to win, got %q", got)
	}
}

func TestDecisionBarFor_DefaultsToShortest(t *testing.T) {
	if got := decisionBarFor("", []string{"1H", "5m", "15m"}); got != "5m" {
		t.Errorf("expected shortest bar (5m), got %q", got)
	}
}

// nextHigherBar drives the 2026-09-21 fix: BTC correlation is measured one timeframe up from the
// decision bar, resolved dynamically against whatever bars are actually maintained — never a
// hardcoded "15m".
func TestNextHigherBar_PicksSmallestStrictlyLonger(t *testing.T) {
	if got := nextHigherBar("5m", []string{"5m", "15m", "1H", "4H"}); got != "15m" {
		t.Errorf("expected 15m (smallest bar strictly longer than 5m), got %q", got)
	}
}

// The whole point of resolving this dynamically: if the decision timeframe itself moves to 15m,
// "one step up" must automatically become 1H — no code change, no re-derivation of a constant.
func TestNextHigherBar_TracksTheDecisionBarMovingUp(t *testing.T) {
	if got := nextHigherBar("15m", []string{"5m", "15m", "1H", "4H"}); got != "1H" {
		t.Errorf("expected 1H once the decision bar itself is 15m, got %q", got)
	}
}

func TestNextHigherBar_NoBarLongerThanDecision_ReturnsEmpty(t *testing.T) {
	if got := nextHigherBar("4H", []string{"5m", "15m", "1H", "4H"}); got != "" {
		t.Errorf("expected no higher bar available, got %q", got)
	}
}

func TestNextHigherBar_IgnoresBarsAtOrBelowDecision(t *testing.T) {
	// 5m and 15m (== and < the decision bar) must never be selected, only 1H.
	if got := nextHigherBar("15m", []string{"5m", "15m", "1H"}); got != "1H" {
		t.Errorf("expected 1H, got %q (a bar at or below the decision bar was wrongly picked)", got)
	}
}

func TestNextHigherBar_EmptyAvailableList(t *testing.T) {
	if got := nextHigherBar("5m", nil); got != "" {
		t.Errorf("expected empty result with no bars available, got %q", got)
	}
}

func TestApplyCandle_TrimsToWindowLimit(t *testing.T) {
	var mu sync.Mutex
	candles := map[string][]domain.Candle{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		applyCandle(&mu, candles, "1m", domain.Candle{Timestamp: base.Add(time.Duration(i) * time.Minute), Close: dec("1")}, 3)
	}
	if got := len(candles["1m"]); got != 3 {
		t.Fatalf("expected window trimmed to 3, got %d", got)
	}
}

func TestApplyCandle_ReplacesLastEntryOnSameTimestamp(t *testing.T) {
	var mu sync.Mutex
	candles := map[string][]domain.Candle{}
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	applyCandle(&mu, candles, "1m", domain.Candle{Timestamp: ts, Close: dec("1")}, 10)
	applyCandle(&mu, candles, "1m", domain.Candle{Timestamp: ts, Close: dec("2")}, 10)
	if got := len(candles["1m"]); got != 1 {
		t.Fatalf("expected the second push to replace, not append; got %d entries", got)
	}
	if !candles["1m"][0].Close.Equal(dec("2")) {
		t.Fatalf("expected the replaced close to be 2, got %s", candles["1m"][0].Close)
	}
}
