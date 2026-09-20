package okx

import (
	"fmt"
	"hash/crc32"
	"sort"
	"strconv"
	"strings"
)

// Package-level: the order-book snapshot+delta merge for OKX's `books` channel (up to 400
// levels/side, vs. `books5`'s hard-capped 5 — CLAUDE.md's Trade page fixes, 2026-09-19). Confirmed
// live before writing this: `books` is public (no login, same wseea.okx.com host every other public
// socket already uses), pushes an `action:"snapshot"` full book once per subscription then
// `action:"update"` deltas, and both carry a `checksum` field.
//
// This package holds the PURE merge/checksum logic, deliberately with no WebSocket dependency, so
// it is unit-testable against scripted messages rather than a live socket (the same "extract the
// pure logic, keep the IO thin" pattern this codebase already uses throughout, e.g.
// internal/optimizer/scoring.go).

// BookLevel is one price level, kept as the RAW WIRE STRINGS OKX sent — not parsed to a numeric
// type — because the checksum is computed over those exact strings (CRC32 of "price:size" pairs,
// per OKX's own documented algorithm) and re-serializing a parsed decimal could format a price
// differently than OKX's own string (trailing zeros, exponent notation) and silently break every
// checksum. The price is ALSO kept parsed as a sortable key (priceKey) so the merge can order
// levels without repeatedly re-parsing the string on every operation.
type BookLevel struct {
	Px       string
	Sz       string
	priceKey float64
}

// OrderBook is one instrument's live, merged order book.
type OrderBook struct {
	asks map[string]string // price string -> size string
	bids map[string]string
}

func newOrderBook() *OrderBook {
	return &OrderBook{asks: make(map[string]string), bids: make(map[string]string)}
}

// WireLevel mirrors one row of OKX's `books`/`books5` payload: [price, size, deprecated, numOrders].
type WireLevel [4]string

// BooksPush mirrors one `data[]` entry of the `books` channel's snapshot/update push.
type BooksPush struct {
	Asks     []WireLevel `json:"asks"`
	Bids     []WireLevel `json:"bids"`
	Ts       string      `json:"ts"`
	Checksum int32       `json:"checksum"`
}

// BookMerger owns one instrument's live OrderBook plus enough state to validate every update
// against OKX's own checksum. NOT safe for concurrent use — callers serialize access per
// instrument (matching every other per-instrument WS handler in this codebase, which already
// process one instrument's messages from a single dispatch goroutine).
type BookMerger struct {
	book *OrderBook
}

// NewBookMerger returns a merger with no book yet — the first Apply must carry action="snapshot".
func NewBookMerger() *BookMerger {
	return &BookMerger{}
}

// Apply merges one push into the book. action must be "snapshot" (replaces the book entirely) or
// "update" (applies deltas: a size of "0" removes that price level, anything else upserts it).
// Returns an error if the resulting book's checksum does not match push.Checksum — the caller's job
// on that error is to drop this book and resubscribe (a fresh snapshot corrects any drift), never
// to keep serving a possibly-corrupt book (docs/MANUAL_TRADE_PLAN.md §7's explicit requirement).
func (m *BookMerger) Apply(action string, push BooksPush) error {
	switch action {
	case "snapshot":
		m.book = newOrderBook()
		applyRows(m.book.asks, push.Asks)
		applyRows(m.book.bids, push.Bids)
	case "update":
		if m.book == nil {
			return fmt.Errorf("update received before any snapshot")
		}
		applyRows(m.book.asks, push.Asks)
		applyRows(m.book.bids, push.Bids)
	default:
		return fmt.Errorf("unknown books action %q", action)
	}

	if push.Checksum != 0 {
		got := m.checksum()
		if got != push.Checksum {
			return fmt.Errorf("checksum mismatch: computed %d, OKX sent %d", got, push.Checksum)
		}
	}
	return nil
}

func applyRows(side map[string]string, rows []WireLevel) {
	for _, r := range rows {
		px, sz := r[0], r[1]
		if sz == "0" {
			delete(side, px)
			continue
		}
		side[px] = sz
	}
}

// TopN returns the top n levels of each side, asks ascending (best/lowest first) and bids
// descending (best/highest first) — the standard convention every consumer of this data (the
// panel's ladder, any future depth calc) expects.
func (m *BookMerger) TopN(n int) (asks, bids []BookLevel) {
	if m.book == nil {
		return nil, nil
	}
	return topLevels(m.book.asks, n, true), topLevels(m.book.bids, n, false)
}

func topLevels(side map[string]string, n int, ascending bool) []BookLevel {
	out := make([]BookLevel, 0, len(side))
	for px, sz := range side {
		f, err := strconv.ParseFloat(px, 64)
		if err != nil {
			continue // a price OKX itself sent that doesn't parse is not something to guess at
		}
		out = append(out, BookLevel{Px: px, Sz: sz, priceKey: f})
	}
	sort.Slice(out, func(i, j int) bool {
		if ascending {
			return out[i].priceKey < out[j].priceKey
		}
		return out[i].priceKey > out[j].priceKey
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// checksum reproduces OKX's own documented algorithm: interleave the top 25 levels of each side
// (ask, bid, ask, bid, ...) as "price:size" pairs joined by colons, CRC32 the resulting string.
// Fewer than 25 levels on one side simply contributes nothing further from that side — OKX's own
// documented behavior, not a special case this code invents.
func (m *BookMerger) checksum() int32 {
	asks := topLevels(m.book.asks, 25, true)
	bids := topLevels(m.book.bids, 25, false)

	var parts []string
	for i := 0; i < 25; i++ {
		if i < len(bids) {
			parts = append(parts, bids[i].Px+":"+bids[i].Sz)
		}
		if i < len(asks) {
			parts = append(parts, asks[i].Px+":"+asks[i].Sz)
		}
	}
	joined := strings.Join(parts, ":")
	// OKX's checksum is a SIGNED 32-bit CRC32 (documented as such — the raw CRC32 is unsigned and
	// must be reinterpreted as int32, not merely cast, to match values OKX sends above 2^31-1).
	return int32(crc32.ChecksumIEEE([]byte(joined)))
}
