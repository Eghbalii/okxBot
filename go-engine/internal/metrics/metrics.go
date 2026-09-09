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

	// The exchange-side SL/TP protection counters (2026-09-09). These exist because the failure
	// this mechanism prevents is SILENT: a real position running without a stop looks exactly like
	// one running with a stop, right up until it doesn't. RealProtectionMissingTotal is the one to
	// alert on — any nonzero value means a live position was found unprotected, which should be
	// impossible if placement and cancellation are both working.
	RealProtectionPlacedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_real_protection_placed_total",
		Help: "Resting SL/TP (algo) orders successfully placed on the exchange, by instrument.",
	}, []string{"inst_id"})

	RealProtectionFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_real_protection_failed_total",
		Help: "Failed attempts to rest an SL/TP order on the exchange, by instrument.",
	}, []string{"inst_id"})

	RealProtectionAmendedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_real_protection_amended_total",
		Help: "Resting SL/TP orders successfully amended on the exchange, by instrument.",
	}, []string{"inst_id"})

	RealProtectionAmendFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_real_protection_amend_failed_total",
		Help: "Failed attempts to amend a resting SL/TP order, by instrument.",
	}, []string{"inst_id"})

	// RealProtectionMissingTotal counts open real positions found with no live protective order on
	// the exchange during verification — each one a position that was running unprotected.
	RealProtectionMissingTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_real_protection_missing_total",
		Help: "Open real positions found without a live exchange-side SL/TP order, by instrument.",
	}, []string{"inst_id"})

	// RealUnprotectedClosedTotal counts positions flattened immediately after opening because their
	// protection could not be placed — the deliberate "never hold an unprotected real position"
	// response (2026-09-09 decision).
	RealUnprotectedClosedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_real_unprotected_closed_total",
		Help: "Real positions closed immediately because exchange-side SL/TP could not be placed.",
	}, []string{"inst_id"})

	// RealEarlyCloseIgnoredTotal counts model early-close requests real trading declined because
	// trading.allow_rl_early_close is off (2026-09-08). A growing count is not a fault — it is the
	// evidence for whether that action is worth enabling: pair it with how those positions
	// actually resolved (SL, TP, or timeout) to judge whether the model was right to want out.
	RealEarlyCloseIgnoredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_real_early_close_ignored_total",
		Help: "Model early-close requests ignored in real trading because the action is disabled, by instrument.",
	}, []string{"inst_id"})

	// KafkaStaleMessagesTotal counts messages dropped for being older than the consumer's
	// MaxMessageAge (internal/kafkastream). A nonzero, GROWING value means a consumer is working
	// through a backlog of market data too old to act on — normal and self-clearing right after a
	// broker restart or a fresh consumer group, but a sustained rate means a consumer is genuinely
	// falling behind and its instrument's decisions are being made on stale prices.
	KafkaStaleMessagesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_kafka_stale_messages_total",
		Help: "Kafka messages dropped for exceeding the consumer's max message age, by topic and group.",
	}, []string{"topic", "group"})

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

	// ModelOpenDecisionsTotal counts every open/skip decision the RL model returns for a strategy
	// signal (CLAUDE.md §15.12's openDecision) — distinct from StrategySignalsTotal above, which
	// counts a strategy's own opinion regardless of whether the model (or rl_sizing) is even in the
	// loop. Only incremented when the model was actually called (RLSizing enabled and a category
	// resolved), so this reads zero while rl_sizing is off rather than looking like the model is
	// silently declining everything.
	ModelOpenDecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_model_open_decisions_total",
		Help: "Total number of RL model open/skip decisions on strategy signals, by instrument and decision (open/skip).",
	}, []string{"inst_id", "decision"})

	// ControllerUpdatesTotal counts every update the SignalConductor decides is due (CLAUDE.md
	// §15.12's ShouldUpdate) — i.e. how many times the lifecycle actually asks the model about an
	// open position, regardless of what the model then answers.
	ControllerUpdatesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_controller_updates_total",
		Help: "Total number of in-trade update decisions triggered by the SignalConductor, by instrument.",
	}, []string{"inst_id"})

	// ModelUpdateDecisionsTotal counts what the RL model answered on each controller-triggered
	// update call (none/update/close) — pairs with ControllerUpdatesTotal to show how the model's
	// answers break down, not just how often it was asked.
	ModelUpdateDecisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "okxbot_model_update_decisions_total",
		Help: "Total number of RL model update decisions on open positions, by instrument and decision (none/update/close).",
	}, []string{"inst_id", "decision"})
)
