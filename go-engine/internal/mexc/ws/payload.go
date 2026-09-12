package ws

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/shopspring/decimal"
)

// TickerPush is the payload of a push.ticker message (verified live 2026-09-13).
type TickerPush struct {
	Symbol    string      `json:"symbol"`
	LastPrice json.Number `json:"lastPrice"`
	Bid1      json.Number `json:"bid1"`
	Ask1      json.Number `json:"ask1"`
	High24    json.Number `json:"high24Price"`
	Lower24   json.Number `json:"lower24Price"`
	Volume24  json.Number `json:"volume24"`
	Timestamp int64       `json:"timestamp"`
}

// DecodeTicker converts a push.ticker message into a domain Ticker.
func DecodeTicker(m Message) (domain.Ticker, error) {
	var p TickerPush
	if err := json.Unmarshal(m.Data, &p); err != nil {
		return domain.Ticker{}, fmt.Errorf("decode mexc ticker push: %w", err)
	}
	return domain.Ticker{
		InstID:  p.Symbol,
		Last:    num(p.LastPrice),
		BidPx:   num(p.Bid1),
		AskPx:   num(p.Ask1),
		High24h: num(p.High24),
		Low24h:  num(p.Lower24),
		Vol24h:  num(p.Volume24),
	}, nil
}

// KlinePush is the payload of a push.kline message (verified live 2026-09-13):
//
//	{"symbol":"BTC_USDT","interval":"Min5","t":1789248000,"o":77135.2,"c":77135.2,
//	 "h":77135.2,"l":77135.1,"a":1352118.06,"q":175292, ...}
//
// t is the bar's OPEN time in SECONDS; q is contract volume; a is quote-currency amount.
type KlinePush struct {
	Symbol   string      `json:"symbol"`
	Interval string      `json:"interval"`
	T        int64       `json:"t"`
	O        json.Number `json:"o"`
	C        json.Number `json:"c"`
	H        json.Number `json:"h"`
	L        json.Number `json:"l"`
	Q        json.Number `json:"q"`
}

// DecodeKline converts a push.kline message into a domain Candle.
//
// IMPORTANT — the candle returned is the FORMING bar, not a finalized one.
//
// MEXC re-pushes the current bar on every update with no "this bar is closed" marker, where OKX's
// candle channel carries an explicit confirm flag ("1" = finalized) that this project's ingestor
// keys on. There is no equivalent field to read here; the only way to know a MEXC bar has closed is
// to observe a push whose open-time `t` is NEWER than the one before it, at which point the PREVIOUS
// bar is final.
//
// That is deliberately NOT hidden inside this decoder. Finalization is per-subscription state
// (which bar was last seen) and belongs to the component tracking the stream — see KlineFinalizer
// below — because a decoder that silently held state would make two subscriptions to the same
// symbol interfere with each other. Callers that persist candles or trigger strategy evaluation
// must use the finalizer; treating every push as a closed bar would re-evaluate strategies
// several times per second on an unfinished candle, which CLAUDE.md §14 is explicit is the wrong
// behaviour (strategies are candle-CLOSE driven).
func DecodeKline(m Message) (domain.Candle, error) {
	var p KlinePush
	if err := json.Unmarshal(m.Data, &p); err != nil {
		return domain.Candle{}, fmt.Errorf("decode mexc kline push: %w", err)
	}
	return domain.Candle{
		Timestamp: time.Unix(p.T, 0).UTC(),
		Open:      num(p.O),
		High:      num(p.H),
		Low:       num(p.L),
		Close:     num(p.C),
		Volume:    num(p.Q),
	}, nil
}

// KlineFinalizer turns MEXC's stream of forming-candle pushes into finalized candles.
//
// This is the piece that reconciles MEXC's protocol with what the rest of this system expects. OKX
// says "this bar is done" with a confirm flag; MEXC never says it, so closure is inferred: when a
// push arrives for a NEWER bar than the one being tracked, the tracked bar is complete and is
// emitted exactly once.
//
// Keyed by (symbol, interval) because one process subscribes to several instruments and several
// timeframes on the same connection (CLAUDE.md §9's multi-timeframe design), and a single shared
// "last bar" would let a 5m push finalize a 1H candle.
//
// A restart loses the in-flight bar, which is correct rather than unfortunate: that bar is
// re-pushed by MEXC on reconnect and finalized normally when the next one opens. Emitting a
// partially-observed bar as final after a restart would write a candle whose high/low reflect only
// the fraction of the period this process happened to be connected for.
type KlineFinalizer struct {
	// last maps (symbol, interval) to the most recent forming bar seen for it.
	last map[string]domain.Candle
}

// NewKlineFinalizer creates a finalizer.
func NewKlineFinalizer() *KlineFinalizer {
	return &KlineFinalizer{last: make(map[string]domain.Candle)}
}

// Observe records a forming-candle push and reports the previous bar if this push closed it.
//
// Returns (candle, true) exactly once per completed bar, and (zero, false) for every push that
// merely updates the bar in progress. Not safe for concurrent use by design — one finalizer belongs
// to one stream, and the WS client's dispatch goroutine is single-threaded, so a mutex here would
// be protecting against a caller that does not exist while implying it is safe to share one.
func (f *KlineFinalizer) Observe(symbol, interval string, c domain.Candle) (domain.Candle, bool) {
	key := symbol + "|" + interval
	prev, seen := f.last[key]
	f.last[key] = c

	if !seen {
		// First push for this stream: nothing has closed yet. The bar in progress is now tracked.
		return domain.Candle{}, false
	}
	if c.Timestamp.After(prev.Timestamp) {
		// A newer bar opened, so the one we were tracking is final.
		return prev, true
	}
	// Same bar updating, or (defensively) an out-of-order push for an older bar, which is never
	// treated as a finalization — that would emit a bar already emitted.
	return domain.Candle{}, false
}

// num converts a JSON number's literal text to a decimal, preserving the digits the exchange sent.
func num(n json.Number) decimal.Decimal {
	if n == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(n.String())
	if err != nil {
		return decimal.Zero
	}
	return d
}
