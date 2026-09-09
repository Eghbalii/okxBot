package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/eghbalii/okxBot/go-engine/internal/okx/ws"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// The private-WebSocket bridge: OKX pushes this account's position/order/balance changes, and this
// republishes them onto the event bus so cmd/trader learns about a fill, a liquidation, a triggered
// stop, or a manual close from OKX's own app the moment it happens (2026-09-09 request) — instead
// of only on the next reconciliation poll.
//
// WHY THIS LIVES IN THE GATEWAY. The private WS requires OKX credentials, and this gateway is the
// only process that holds them (CLAUDE.md §27.1). Giving cmd/trader its own socket would mean
// giving it back the credentials that were deliberately moved out of it, widening the blast radius
// §16.10 argued for narrowing. Kafka is already this project's service-to-service bus, so the
// socket terminates here and the events travel the way every other event does.
//
// The events carry no position DATA, only the instrument that changed. cmd/trader's reconciliation
// re-reads both sides authoritatively when it runs, so shipping OKX's payload across would create a
// second, independently-decoded view of a position that could disagree with the one the trading
// logic actually acts on. "Something changed on BTC, go look" is the whole message.

// TopicAccountEvents is the bus topic private-WebSocket account events are published to.
const TopicAccountEvents = "okx.account-events"

// accountStream republishes OKX private-channel pushes onto the event bus.
type accountStream struct {
	client    *ws.PrivateClient
	publisher port.MarketDataPublisher
	// instIDToSymbol maps OKX's execution instId back to the short internal symbol every other
	// service keys on (CLAUDE.md §33.4). A push naming an unmapped instrument is published under
	// its raw instId rather than dropped — an event for an instrument this bot does not recognise
	// is exactly the kind of thing an operator needs to see, not something to filter away.
	instIDToSymbol map[string]string
	logger         *slog.Logger
}

// accountEvent is what lands on the bus. InstID is the short symbol so consumers can route it with
// the same key they use for every other topic.
type accountEvent struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

// handle decodes one private push and publishes one event per distinct instrument it names.
//
// A single push can carry several positions (OKX batches them), so this de-duplicates by
// instrument: three updates to the same position in one frame are one "go look at BTC", not three.
func (a *accountStream) handle(ctx context.Context, msg ws.Message) {
	var rows []struct {
		InstID string `json:"instId"`
	}
	if err := json.Unmarshal(msg.Data, &rows); err != nil {
		a.logger.Warn("account stream: could not decode push", "channel", msg.Arg.Channel, "error", err)
		return
	}

	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.InstID == "" {
			// The balance channel pushes account-wide rows with no instrument. Nothing to route,
			// and the periodic reconcile already refreshes balance on its own cadence.
			continue
		}
		symbol := row.InstID
		if mapped, ok := a.instIDToSymbol[row.InstID]; ok {
			symbol = mapped
		}
		if _, dup := seen[symbol]; dup {
			continue
		}
		seen[symbol] = struct{}{}

		event := accountEvent{Channel: msg.Arg.Channel, InstID: symbol}
		if err := a.publisher.Publish(ctx, symbol, event); err != nil {
			// Logged, never fatal — the same reliability model every publisher in this codebase
			// uses (CLAUDE.md §12). A dropped event costs latency, not correctness: the periodic
			// reconciliation poll still catches the change.
			a.logger.Warn("account stream: publish failed", "channel", msg.Arg.Channel, "instId", symbol, "error", err)
			continue
		}
		a.logger.Info("account event pushed by the exchange", "channel", msg.Arg.Channel, "instId", symbol)
	}
}

// Run streams until ctx is cancelled. The client reconnects on its own, so this returns only when
// the context ends.
func (a *accountStream) Run(ctx context.Context) error {
	a.client.Handler = func(msg ws.Message) { a.handle(ctx, msg) }
	return a.client.Run(ctx)
}

// reverseSymbolMap inverts symbol -> instId into instId -> symbol, so a push naming an execution
// instrument can be routed back to the symbol the rest of the system keys on.
func reverseSymbolMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for symbol, instID := range m {
		out[instID] = symbol
	}
	return out
}
