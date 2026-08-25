// Package metrics defines the Prometheus metrics exported by go-engine services (scraped by
// Prometheus/Grafana, CLAUDE.md §11). Register once per process via promhttp.Handler().
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// StrategySignalsTotal counts every strategy evaluation result, including holds.
	StrategySignalsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_strategy_signals_total",
		Help: "Total number of strategy signal evaluations, by strategy/instrument/side.",
	}, []string{"strategy", "inst_id", "side"})

	// PaperOrdersOpenedTotal counts virtual orders opened by the Paper Trading Engine.
	PaperOrdersOpenedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_paper_orders_opened_total",
		Help: "Total number of paper (virtual) orders opened, by strategy/instrument/side.",
	}, []string{"strategy", "inst_id", "side"})

	// PaperOrdersClosedTotal counts closed paper orders by close reason (sl/tp/manual/timeout) —
	// this is the SL-hit / TP-hit counter the dashboard needs.
	PaperOrdersClosedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_paper_orders_closed_total",
		Help: "Total number of paper orders closed, by instrument and close reason (sl/tp/manual/timeout).",
	}, []string{"inst_id", "reason"})

	// PaperOrdersOpenGauge tracks currently-open paper orders per instrument.
	PaperOrdersOpenGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "okxbot_paper_orders_open",
		Help: "Current number of open paper orders, by instrument.",
	}, []string{"inst_id"})

	// PaperOrdersRealizedPnL accumulates realized PnL (USD) from closed paper orders. A Gauge
	// (not Counter) because realized PnL can decrease on a losing trade.
	PaperOrdersRealizedPnL = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "okxbot_paper_orders_realized_pnl_usd_total",
		Help: "Cumulative realized PnL (USD) from closed paper orders, by instrument.",
	}, []string{"inst_id"})

	// IngestorEventsTotal counts ticks/candles received from OKX WS, by kind (tick/candle).
	IngestorEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_ingestor_events_total",
		Help: "Total number of market data events received from OKX WS, by kind and instrument.",
	}, []string{"kind", "inst_id"})

	// WSConnected reports whether a given OKX WS connection is currently up (1) or down (0),
	// labeled by URL+channel — the liveness check: `okxbot_ws_connected == 0` means that
	// connection is currently disconnected (auto-reconnect is retrying in the background).
	WSConnected = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "okxbot_ws_connected",
		Help: "Whether an OKX WebSocket connection is currently up (1) or down (0), by url/channel.",
	}, []string{"url", "channel"})

	// WSReconnectsTotal counts every reconnect attempt for a given OKX WS connection — a
	// nonzero/increasing rate indicates connection instability worth investigating even if the
	// gauge above shows "connected" right now.
	WSReconnectsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_ws_reconnects_total",
		Help: "Total number of reconnect attempts for an OKX WebSocket connection, by url/channel.",
	}, []string{"url", "channel"})
)
