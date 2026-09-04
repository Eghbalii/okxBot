// Package config loads runtime configuration from environment variables and a YAML file.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// Config holds all settings for the go-engine services (ingestor + trader).
type Config struct {
	OKX struct {
		APIKey        string `yaml:"-"`
		APISecret     string `yaml:"-"`
		APIPassphrase string `yaml:"-"`
		Simulated     bool   `yaml:"-"`
		RESTBaseURL   string `yaml:"rest_base_url"`
		PublicWSURL   string `yaml:"public_ws_url"`
		BusinessWSURL string `yaml:"business_ws_url"`
		PrivateWSURL  string `yaml:"private_ws_url"`
	} `yaml:"okx"`

	// Redis is still used by internal/optimizer.TrialStore for disposable trial state (CLAUDE.md
	// §16.3) — unrelated to the event bus, which now runs on Kafka (below).
	Redis struct {
		Addr string `yaml:"-"`
	} `yaml:"redis"`

	// Kafka configures the internal event bus (CLAUDE.md §12): ticks/candles/paper-order events
	// flow through Kafka topics (internal/kafkastream), replacing the earlier Redis Streams bus.
	Kafka struct {
		Brokers []string `yaml:"-"`
	} `yaml:"kafka"`

	Postgres struct {
		DSN string `yaml:"-"`
	} `yaml:"postgres"`

	RLService struct {
		URL string `yaml:"-"`
	} `yaml:"rl_service"`

	Trading struct {
		// InstIDs is now a list of SHORT INTERNAL SYMBOLS ("BTC", "ETH", "DOGE", ...), not OKX's
		// own instId strings — explicit operator design (2026-09-04, CLAUDE.md §27): this bot only
		// trades USD-quoted perpetual futures, so every service/DB row/config entry only ever needs
		// to say WHICH TOKEN, never OKX's full wire-format instId (which additionally varies by
		// product — "BTC-USDT-SWAP" vs. "BTC-USD_UM_XPERP-<expiry-date>" — and, for the X-Perp
		// product, changes over time as OKX rolls the contract's expiry). SymbolMap (below) is the
		// one place a short symbol resolves to a real OKX instId; every other line of code in this
		// project (candles, paper_orders, strategy_assignments, Kafka topic keys, the panel) carries
		// the short symbol and never touches OKX's wire format at all.
		InstIDs         []string        `yaml:"inst_ids"`
		// SymbolMap resolves each InstIDs entry to the real OKX instId a WebSocket subscription or
		// REST call actually needs (internal/okx.SymbolMap — see its own doc comment). Required:
		// every configured symbol must have an entry, or the affected service refuses to start
		// rather than silently subscribing to/calling nothing for it (the same "loud failure over
		// a data gap that looks healthy" principle as CLAUDE.md §9's bar-casing validation).
		SymbolMap       map[string]string `yaml:"symbol_map"`
		PollIntervalSec int             `yaml:"poll_interval_sec"`
		TdMode          string          `yaml:"td_mode"`       // "cross" or "isolated"
		PosMode         string          `yaml:"pos_mode"`      // "net" or "long_short" (hedge mode)
		MinOrderUSD     decimal.Decimal `yaml:"min_order_usd"` // skip rebalancing orders smaller than this
		// AllowRealMoney must be explicitly true before cmd/trader will run against real (non-demo)
		// OKX credentials — it refuses to start otherwise (CLAUDE.md §15.6's paper -> demo -> real
		// progression). This exists because reaching real trading by simply *not setting*
		// OKX_SIMULATED_TRADING would make an unset env var the difference between a sandbox and
		// real capital; going live should require saying so.
		AllowRealMoney bool `yaml:"allow_real_money"`
		// URL is cmd/trader's own restart-only HTTP surface as reached from cmd/api (env only, same
		// pattern as PaperTrading.URL/Tester.URL/RLService.URL) — used to proxy the panel's Real-tab
		// restart button through cmd/api (CLAUDE.md §11 / real-trading readiness plan, 2026-09-04).
		URL string `yaml:"-"`
		// UseConductorLifecycle switches cmd/trader from the old flat delta-notional rebalance loop
		// (usecase.Trader) to usecase.RealTrader — the strategy-signal + conductor-mediated
		// lifecycle usecase.PaperTrader already runs, adapted for real orders (CLAUDE.md §27's
		// real-trading plan, commit 8). Defaults to false/off: the old Trader remains production
		// behavior until this is explicitly flipped, matching every other RL feature flag's
		// "additive, never required until opted in" posture in this codebase
		// (RLSizing/RLSLTPAdjust/etc.) — this is the riskiest commit in that plan to get wrong
		// silently, so an explicit off-by-default flag is the safety valve.
		UseConductorLifecycle bool `yaml:"use_conductor_lifecycle"`
		// SafeMoneyUSD is a reserve subtracted from the exchange's reported real balance before
		// RealTrader ever sizes a position or records equity for the panel (CLAUDE.md real-trading
		// readiness plan, 2026-09-04 operator decision) — capital the operator wants to stay
		// untouched even if every open position were liquidated, since isolated margin (§27.2)
		// bounds a position's own loss to its own margin, never the whole account, so money set
		// aside this way is genuinely safe from liquidation, not just from this engine's own
		// sizing. Zero (default) uses the full reported balance, matching pre-existing behavior.
		SafeMoneyUSD decimal.Decimal `yaml:"safe_money_usd"`

		// ExecInstType/ExecSettleCcy answer "which instType/currency does account/position/balance
		// access use" — separate from SymbolMap because these are account-wide properties, not
		// per-symbol ones (every X-Perp instrument on this account shares instType=FUTURES and
		// settles in the same currency). Found load-bearing 2026-09-04 (CLAUDE.md §27): this
		// project's real account settles in USDC under Multi-currency margin mode, and its
		// tradeable instruments report instType=FUTURES, not the SWAP/USDT this codebase assumed
		// pre-2026-09-04. Both empty by default (RealTrader's own accessors fall back to
		// instType=SWAP, currency=USDT) — i.e. no override needed for a deployment whose account
		// trades the classic SWAP product directly.
		ExecInstType  string `yaml:"exec_inst_type"`
		ExecSettleCcy string `yaml:"exec_settle_ccy"`
	} `yaml:"trading"`

	// Ingestion controls cmd/ingestor: the always-on, broad set of candle timeframes it collects
	// from OKX and publishes to Redis, independent of what any given paper-trading/strategy test
	// run actually evaluates (see PaperTrading.Bars).
	Ingestion struct {
		Bars []string `yaml:"bars"`
	} `yaml:"ingestion"`

	PaperTrading struct {
		// URL is cmd/paper-trader's own control-box HTTP surface as reached from cmd/api (env only,
		// same pattern as Tester.URL/RLService.URL) — used to proxy the panel's restart button
		// through cmd/api rather than exposing this service directly (CLAUDE.md §11).
		URL           string          `yaml:"-"`
		NotionalUSD   decimal.Decimal `yaml:"notional_usd"`
		MaxOpenOrders int             `yaml:"max_open_orders"`
		// Bars is the subset of Ingestion.Bars that cmd/paper-trader actually maintains candle
		// windows for and evaluates strategies against — changeable per test run without
		// touching the always-on ingestor. Must be a subset of Ingestion.Bars (see
		// ValidatePaperTradingBars); a bar not being collected by the ingestor has no Redis
		// stream to consume from.
		Bars        []string `yaml:"bars"`
		CandleLimit int      `yaml:"candle_limit"`
		// RLSLTPAdjust enables the RL-driven in-trade SL/TP adjustment pass (CLAUDE.md §15.4). Off
		// by default: PaperTrader runs exactly as it did before §15 wherever this is false, since a
		// meaningful decision here requires a trained (or at least deliberately no-op) RL model to
		// be reachable at RLService.URL.
		RLSLTPAdjust bool `yaml:"rl_sltp_adjust"`
		// RLSizing lets the RL agent size each new paper order (notional + leverage) from its
		// TargetExposure/LeverageFrac action instead of opening every order at NotionalUSD and 1x
		// (CLAUDE.md §15.4). Off by default, and independent of RLSLTPAdjust — but note that while
		// it's off, the paper_orders log records no leverage or exposure variance whatsoever, which
		// is the training signal §15.8's continued-live-learning phase needs in order to learn
		// sizing at all. Turn it on once a model with the current action schema is actually loaded.
		RLSizing bool `yaml:"rl_sizing"`
		// RLDecisionBar picks which timeframe's strategy signals/price context feed a tick-driven
		// RL SL/TP-adjust decision (CLAUDE.md §15.3/§15.9). A tick belongs to no single bar, so this
		// has to be chosen. Empty defaults to the shortest bar in Bars — freshest read of what price
		// is doing right now, which is what an in-trade adjustment reacts to. Must be one of Bars.
		RLDecisionBar string `yaml:"rl_decision_bar"`

		// RLUpdatePnLThresholdPct / RLUpdateMaxInterval drive the signal-lifecycle conductor's
		// update cadence (CLAUDE.md §15.12): an `update` call fires once unrealized PnL has moved
		// this far (as a fraction, 0.01 = 1%) since the last one, OR once this much time has
		// elapsed, OR whenever a strategy fires. PnL-delta rather than a fixed interval is what
		// makes the cadence self-adapting — near-silent while a position ranges, dense while it
		// actually moves — and keeps credit assignment tractable at ~10 meaningful steps per trade
		// instead of thousands of near-identical ones. Zero falls back to conductor's defaults.
		RLUpdatePnLThresholdPct decimal.Decimal `yaml:"rl_update_pnl_threshold_pct"`
		RLUpdateMaxInterval     time.Duration   `yaml:"rl_update_max_interval"`
		// RLEarlyClose lets the model close a position before either level is touched (CLAUDE.md
		// §15.12), recorded as close_reason='rl_early'. Off by default and independent of the other
		// RL flags: it is the one lifecycle action that destroys the counterfactual — a position
		// closed early can never show what it would have done — so it should only be enabled once
		// the SL/TP-adjust behavior is trusted.
		RLEarlyClose bool `yaml:"rl_early_close"`
		// RLClamps bound where the model may PLACE stops and targets on an open (CLAUDE.md
		// §15.11/§15.12). Distinct from the ratchet, which governs how levels may MOVE later and
		// says nothing about the initial placement. Early in training the policy is effectively
		// random: a stop 0.001% from entry stops out on noise, one 40% away turns a bounded loss
		// into an account event, and a target nearer than the stop is negative-expectancy by
		// construction. Zero disables the respective clamp.
		RLClamps struct {
			MinSLDistPct decimal.Decimal `yaml:"min_sl_dist_pct"`
			MaxSLDistPct decimal.Decimal `yaml:"max_sl_dist_pct"`
			// MaxLossPct bounds the REALIZED loss a stop can produce once leverage is applied
			// (2026-08-31 request: "SL should never allow more than 15% loss, at any leverage, no
			// cap on profit") — distinct from MaxSLDistPct above, which only bounds the raw price
			// distance and knows nothing about leverage. At high leverage MaxSLDistPct alone could
			// still let a stop realize far more than 15% of margin; this narrows the effective
			// price-distance bound to MaxLossPct/leverage whenever that is tighter. Zero disables it.
			MaxLossPct   decimal.Decimal `yaml:"max_loss_pct"`
			MinTPSLRatio decimal.Decimal `yaml:"min_tp_sl_ratio"`
		} `yaml:"rl_clamps"`
		// RLMaxOpenDuration force-closes any position open longer than this, close_reason='timeout'
		// (CLAUDE.md §15.14, operator request 2026-08-30: positions were observed sitting open a
		// long time with barely-moving PnL, tying up an instrument's one-open-position slot,
		// §16.9, without going anywhere). A hard housekeeping limit, not a model decision — fires
		// unconditionally, independent of the RL* flags above. Zero falls back to
		// conductor.DefaultMaxOpenDuration (6h).
		RLMaxOpenDuration time.Duration `yaml:"rl_max_open_duration"`
	} `yaml:"paper_trading"`

	// Account is the shared capital pool every token trades against (CLAUDE.md §15.6, revised
	// 2026-08-28 — this replaced per-token sub-budgets). Applies across all three modes, which is
	// why it isn't nested under paper_trading.
	Account struct {
		// InitialUSD is the starting balance, and what a drained paper/demo account resets back to.
		// Real mode never auto-resets (§15.7).
		InitialUSD decimal.Decimal `yaml:"initial_usd"`
		// MaxPositionPct caps any single position at this fraction of account equity, and
		// MaxTotalExposurePct caps the sum of all open positions. These bound the RL agent's sizing
		// proposal Go-side — the model is never the safety boundary (§5, §15.4). Expressed as
		// fractions (0.25 = 25%), not percentages.
		MaxPositionPct      decimal.Decimal `yaml:"max_position_pct"`
		MaxTotalExposurePct decimal.Decimal `yaml:"max_total_exposure_pct"`
	} `yaml:"account"`

	Risk struct {
		MaxLeverage             decimal.Decimal `yaml:"max_leverage"`
		MaxPositionNotionalUSD  decimal.Decimal `yaml:"max_position_notional_usd"`
		MaxDailyDrawdownPct     decimal.Decimal `yaml:"max_daily_drawdown_pct"`
		MinLiquidationBufferPct decimal.Decimal `yaml:"min_liquidation_buffer_pct"`
	} `yaml:"risk"`

	// API configures cmd/api, the dashboard/reporting backend (CLAUDE.md §11). No auth in v1 — the
	// panel is only reachable over an OpenVPN tunnel into the server's private network, so
	// cmd/api's listener should be bound to that interface, not 0.0.0.0, in production.
	API struct {
		Addr       string   `yaml:"addr"`
		GrafanaURL string   `yaml:"grafana_url"`
		ProcessMgr string   `yaml:"process_manager"` // "systemd" or "docker"
		Units      []string `yaml:"units"`           // systemd unit names or docker container names to report on
	} `yaml:"api"`

	// Optimizer configures cmd/strategy-optimizer (CLAUDE.md §16): the standing service that
	// time-boxes real-market-data trial runs of candidate strategy.Strategy parameter sets per
	// (inst_id, kind) and, on a scheduled interval, persists the best-performing candidate as a
	// new durable sub-strategy row. Deliberately not folded into PaperTrading — this is a
	// separate optimization concern with its own trial-lifecycle bookkeeping (§16.1/§16.2).
	Optimizer struct {
		// URL is the Python/Optuna sidecar's base URL (optimizer-service/, §16's "Optuna sidecar
		// is a brand-new service" decision) — set from env only, matching RLService.URL's pattern.
		URL string `yaml:"-"`
		// Addr is cmd/strategy-optimizer's own HTTP API bind address (POST /optimize, GET
		// /status) — analogous to API.Addr.
		Addr string `yaml:"addr"`
		// ScheduleInterval is a Go duration string (e.g. "24h") on which the built-in scheduler
		// automatically fires one optimization pass per configured Target. This REVISES CLAUDE.md
		// §16.4's original "manually-triggered only" framing — see §16.7.
		ScheduleInterval string `yaml:"schedule_interval"`
		// RunDuration is each optimization run's wall-clock time box (e.g. "4h") — the primary
		// stopping rule (revises §16.3 step 5's "minimum trial count" framing to "time-boxed,
		// with a minimum-trades-per-candidate eligibility floor" — see MinTradesPerCandidate).
		RunDuration string `yaml:"run_duration"`
		// MinTradesPerCandidate is the per-candidate minimum completed trades before it's even
		// eligible to be scored/win at run end (§16.3 step 5's noise-rejection reasoning).
		MinTradesPerCandidate int `yaml:"min_trades_per_candidate"`
		// MinImprovementPct is how many percentage points a winning candidate's win rate must
		// beat the current baseline's win rate by before it's persisted (e.g. 5.0 = must beat
		// baseline by >=5pp). If no baseline exists for a target, MinWinRatePctFloor is used
		// instead (see below) — an explicit, documented implementation choice for one of §16.6's
		// "resolve at implementation time" items.
		MinImprovementPct decimal.Decimal `yaml:"min_improvement_pct"`
		// MinWinRatePctFloor is the win-rate floor a winning candidate must clear when no clean
		// baseline exists to compare against (e.g. a fresh inst_id+kind with no assignment yet).
		MinWinRatePctFloor decimal.Decimal `yaml:"min_win_rate_pct_floor"`
		// Bar is the single candle timeframe each run evaluates candidates against (§16.3: "pick
		// one bar to optimize against per run target ... make it explicit, not hidden").
		Bar string `yaml:"bar"`
		// CandleWindow mirrors PaperTrading.CandleLimit for the optimizer's own candle windows.
		CandleWindow int `yaml:"candle_window"`
		// BatchSize is how many candidate parameter sets are requested from the sidecar at once,
		// refilled as trials complete (§16.3 step 1).
		BatchSize int `yaml:"batch_size"`
		// TrialTTLBufferSec pads a trial's Redis TTL beyond the run's remaining time box, so a
		// stale trial key self-cleans even if the process crashes mid-run (§16.3 step 2's "TTL
		// should exceed the run's remaining time box comfortably").
		TrialTTLBufferSec int `yaml:"trial_ttl_buffer_sec"`
		// Targets is the explicit list of (inst_id, kind) pairs the scheduler optimizes on its
		// interval. Empty means "derive from Trading.InstIDs x DefaultKinds" (see Load below) —
		// kept simple per the "your call" instruction rather than a separate DB-backed table,
		// since this is operator-level config, not per-token runtime state like strategy
		// assignments (§11.3).
		Targets []OptimizerTarget `yaml:"targets"`
		// DefaultKinds is the strategy kinds considered for every Trading.InstIDs entry when
		// Targets is empty.
		DefaultKinds []string `yaml:"default_kinds"`
		// MaxLossPct caps the loss a trial's own SL can realize, as a fraction of entry price
		// (2026-08-31 request: "SL should never allow more than 15% loss, at any leverage" — the
		// optimizer's trials are unleveraged, §16.3, so this is a direct price-distance cap here
		// rather than needing maxSLDistPctFor's leverage division). Applied in signalPrices before
		// a trial is ever opened, mirroring PaperTrading.RLClamps.MaxLossPct/Tester.RLClamps.
		// MaxLossPct's same 15% cap on the two other SL-placing paths — this was the one path that
		// had no cap at all. Zero disables the cap; defaulted to 0.15 below (not opt-in), same
		// reasoning as the other two.
		MaxLossPct decimal.Decimal `yaml:"max_loss_pct"`
	} `yaml:"optimizer"`

	// Tester configures cmd/strategy-tester: a standalone paper-trading copy that opens real
	// positions against live prices to validate strategy signal quality entirely independent of
	// the RL agent and the production paper-trading path (2026-08-30 request) — separate storage
	// (tester_orders/tester_strategy_versions, migration 000010), separate config, separate
	// process, so it can never affect or be affected by cmd/paper-trader.
	Tester struct {
		// URL is cmd/strategy-tester's base URL as reached from cmd/api (env only, same pattern
		// as Optimizer.URL/RLService.URL) — used to proxy the panel's tester tab through cmd/api
		// rather than exposing this service directly (CLAUDE.md §11).
		URL string `yaml:"-"`
		// Addr is cmd/strategy-tester's own HTTP API bind address, analogous to Optimizer.Addr.
		Addr string `yaml:"addr"`
		// Bar is the single decision timeframe this service trades on. Set to "5m" per the
		// operator's own observation (2026-08-30) that signals/fills concentrate there — see
		// CLAUDE.md for the measured evidence (10/10 tokens' single open-position slot filled by
		// 5m, 15m/1H essentially starved).
		Bar string `yaml:"bar"`
		// InstIDs defaults to Trading.InstIDs when empty (Load below) — deliberately the same
		// roster the production system trades, per the operator's explicit "don't limit to one or
		// two tokens, use exactly the same tokens" instruction.
		InstIDs []string `yaml:"inst_ids"`
		// NotionalUSD/Leverage are fixed for every position this service opens — there is no RL
		// sizing here, so "how much" is a flat config value, not a decision (operator's own
		// "size/leverage doesn't matter, start with $10 and 10x" instruction).
		NotionalUSD  decimal.Decimal `yaml:"notional_usd"`
		Leverage     decimal.Decimal `yaml:"leverage"`
		CandleWindow int             `yaml:"candle_window"`
		// MaxOpenDuration force-closes a tester position open longer than this, close_reason=
		// 'timeout' (2026-08-30 request, mirroring paper_trading.rl_max_open_duration/§15.14 for
		// the SAME reason on a SEPARATE config path — this service has no in-trade update
		// mechanic at all, so a position with neither level touched would otherwise sit open
		// forever). Zero falls back to a 6h default at construction time, same starting value as
		// production's.
		MaxOpenDuration time.Duration `yaml:"max_open_duration"`

		// Optimize configures cmd/strategy-tester's own automatic per-kind optimization loop
		// (2026-08-31 request): the service proposes new parameter candidates for each strategy
		// kind, judges them by win rate + PnL once enough trades accumulate, and always builds the
		// next candidate from whichever version (including the origin) currently scores best —
		// entirely independent of Optimizer above (separate storage, separate Optuna study
		// namespace via tester.StudyID). Reuses Optimizer.URL's sidecar deployment (the operator's
		// own call: the sidecar is generic and safely shared, studies are isolated per study_id).
		Optimize struct {
			// CheckInterval is how often the loop checks every kind for a judgeable candidate or a
			// free slot to propose a new one (operator's explicit "لوپ رو هر ۱ ساعت اجرا کنیم").
			CheckInterval time.Duration `yaml:"check_interval"`
		} `yaml:"optimize"`

		// RLClamps bounds this service's own opened SL levels the same way PaperTrading.RLClamps
		// does production's (2026-08-31 request) — this service previously had NO clamp/EnsureStop
		// pass at all (deliberately, since it measures raw strategy signal quality), which also
		// meant a strategy's own SL had no leverage-aware loss ceiling. Only MaxLossPct is actually
		// used here (tester.BuildOrder has no "missing stop" fallback to fill — a signal with no
		// stop is still skipped outright, CLAUDE.md §16.9's rule) — the other fields exist so this
		// struct's shape matches PaperTrading.RLClamps and a future EnsureStop-style fallback could
		// reuse it without a config change.
		RLClamps struct {
			MinSLDistPct decimal.Decimal `yaml:"min_sl_dist_pct"`
			MaxSLDistPct decimal.Decimal `yaml:"max_sl_dist_pct"`
			MaxLossPct   decimal.Decimal `yaml:"max_loss_pct"`
			MinTPSLRatio decimal.Decimal `yaml:"min_tp_sl_ratio"`
		} `yaml:"rl_clamps"`
	} `yaml:"tester"`

	// Gateway configures cmd/okx-gateway, the single process that holds real OKX credentials and
	// rate-limits/prioritizes REST calls across every other service (CLAUDE.md §27.1). Every
	// class left unconfigured (zero fields) falls back to gateway.DefaultLimits() at okx-gateway's
	// own startup — see that function's own doc comment on why the numbers here must be verified
	// against OKX's live docs before real trading, not trusted from this codebase alone.
	Gateway struct {
		// URL is how every OTHER service reaches this gateway (env only, same pattern as
		// RLService.URL/Tester.URL) — once a service migrates to calling the gateway instead of
		// building its own rest.Client (CLAUDE.md §27.1's migration order), it reads this.
		URL string `yaml:"-"`
		// Addr is this service's own HTTP bind address.
		Addr     string            `yaml:"addr"`
		Trade    GatewayClassLimit `yaml:"trade"`
		Leverage GatewayClassLimit `yaml:"leverage"`
		Account  GatewayClassLimit `yaml:"account"`
		Market   GatewayClassLimit `yaml:"market"`
	} `yaml:"gateway"`

	// FillTimeout controls cmd/trader's fill-or-cancel behavior for a placed order (CLAUDE.md
	// §27.5, explicit operator decision): an order not filled within this window is canceled, and
	// the engine does NOT re-price/retry automatically — it waits for the model's next real
	// signal. Futures market orders against a liquid perpetual are expected to fill immediately in
	// the overwhelming majority of cases; this exists to bound the rare case where one doesn't.
	FillTimeout struct {
		OrderFillTimeoutSec int `yaml:"order_fill_timeout_sec"`
	} `yaml:"trading_fill_timeout"`
}

// GatewayClassLimit is one endpoint class's config-overridable rate limit (CLAUDE.md §27.1). A
// zero field means "use gateway.DefaultLimits()' value for this field" — this struct intentionally
// allows overriding only some fields of a class (e.g. just Capacity) rather than requiring every
// field to be set once any override is present.
type GatewayClassLimit struct {
	Capacity   int `yaml:"capacity"`
	Refill     int `yaml:"refill"`
	IntervalMs int `yaml:"interval_ms"`
}

// OptimizerTarget is one (instrument, strategy-kind) pair cmd/strategy-optimizer's scheduler
// optimizes on its configured interval (CLAUDE.md §16.6: "whether cmd/strategy-optimizer runs
// against every configured token/base-strategy pair by default or requires an explicit
// operator-triggered list" — resolved here as an explicit, config-driven list).
type OptimizerTarget struct {
	InstID string `yaml:"inst_id"`
	Kind   string `yaml:"kind"`
}

// Load reads the YAML config at path (if provided) and overlays secrets/endpoints from
// environment variables, which always take precedence.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file %q: %w", path, err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
	}

	cfg.OKX.APIKey = os.Getenv("OKX_API_KEY")
	cfg.OKX.APISecret = os.Getenv("OKX_API_SECRET")
	cfg.OKX.APIPassphrase = os.Getenv("OKX_API_PASSPHRASE")
	cfg.OKX.Simulated = os.Getenv("OKX_SIMULATED_TRADING") == "1"

	if cfg.OKX.RESTBaseURL == "" {
		cfg.OKX.RESTBaseURL = "https://www.okx.com"
	}
	if cfg.OKX.PublicWSURL == "" {
		cfg.OKX.PublicWSURL = "wss://ws.okx.com:8443/ws/v5/public"
	}
	if cfg.OKX.BusinessWSURL == "" {
		cfg.OKX.BusinessWSURL = "wss://ws.okx.com:8443/ws/v5/business"
	}
	if cfg.OKX.PrivateWSURL == "" {
		cfg.OKX.PrivateWSURL = "wss://ws.okx.com:8443/ws/v5/private"
	}
	if cfg.OKX.Simulated {
		// Demo trading uses a dedicated WS host; REST stays on the same host with a header flag.
		cfg.OKX.PublicWSURL = "wss://wspap.okx.com:8443/ws/v5/public"
		cfg.OKX.BusinessWSURL = "wss://wspap.okx.com:8443/ws/v5/business"
		cfg.OKX.PrivateWSURL = "wss://wspap.okx.com:8443/ws/v5/private"
	}

	cfg.Redis.Addr = envOr("REDIS_ADDR", "localhost:6379")
	cfg.Kafka.Brokers = strings.Split(envOr("KAFKA_BROKERS", "localhost:9092"), ",")
	cfg.Postgres.DSN = envOr("POSTGRES_DSN", "postgres://okxbot:okxbot@localhost:5432/okxbot")
	cfg.RLService.URL = envOr("RL_SERVICE_URL", "http://localhost:8000")

	if len(cfg.Trading.InstIDs) == 0 {
		cfg.Trading.InstIDs = []string{"BTC"}
	}
	if cfg.Trading.PollIntervalSec == 0 {
		cfg.Trading.PollIntervalSec = 5
	}
	if cfg.Trading.TdMode == "" {
		cfg.Trading.TdMode = "cross"
	}
	if cfg.Trading.PosMode == "" {
		cfg.Trading.PosMode = "net"
	}
	if cfg.Trading.MinOrderUSD.IsZero() {
		cfg.Trading.MinOrderUSD = decimal.NewFromInt(10)
	}
	if cfg.PaperTrading.NotionalUSD.IsZero() {
		// $40 real account / ~10 configured tokens = $4/token, one open position per token
		// (2026-09-01, explicit operator decision) — see Account.InitialUSD's matching default.
		cfg.PaperTrading.NotionalUSD = decimal.NewFromInt(4)
	}
	if cfg.PaperTrading.MaxOpenOrders == 0 {
		cfg.PaperTrading.MaxOpenOrders = 3
	}
	if len(cfg.Ingestion.Bars) == 0 {
		cfg.Ingestion.Bars = []string{"1m", "3m", "5m", "1H", "4H", "1D"}
	}
	if len(cfg.PaperTrading.Bars) == 0 {
		cfg.PaperTrading.Bars = []string{"1m"}
	}
	if cfg.PaperTrading.CandleLimit == 0 {
		cfg.PaperTrading.CandleLimit = 100
	}
	if cfg.Account.InitialUSD.IsZero() {
		// Lowered 100 -> 40 (2026-09-01, real-money wiring): the real account this service will
		// trade against is funded with $40, shared across paper/demo/real by explicit operator
		// decision so paper training's own sizing stays representative (CLAUDE.md §15.6).
		cfg.Account.InitialUSD = decimal.NewFromInt(40)
	}
	if cfg.Account.MaxPositionPct.IsZero() {
		// 25% of equity in any one position. Deliberately not "whatever the model asks for": at
		// 100x leverage a single uncapped position is an account-ending event, and §5's risk manager
		// only guards the live path, not paper.
		cfg.Account.MaxPositionPct = decimal.NewFromFloat(0.25)
	}
	if cfg.Account.MaxTotalExposurePct.IsZero() {
		// 60% of equity across all open positions combined — the per-position cap alone would still
		// allow four simultaneous 25% positions committing the whole account.
		cfg.Account.MaxTotalExposurePct = decimal.NewFromFloat(0.60)
	}
	if cfg.Risk.MaxLeverage.IsZero() {
		// Lowered 100 -> 10 (2026-09-01, explicit operator decision): OKX rejects leverage above
		// 10x on the real account this service will trade against, so both the RL agent's sizing
		// ceiling and risk.Manager's hard clamp must stay within what the exchange actually
		// accepts. Supersedes §15.4's original 10x-100x framing.
		cfg.Risk.MaxLeverage = decimal.NewFromInt(10)
	}
	if cfg.Risk.MaxPositionNotionalUSD.IsZero() {
		cfg.Risk.MaxPositionNotionalUSD = decimal.NewFromInt(1000)
	}
	if cfg.Risk.MaxDailyDrawdownPct.IsZero() {
		cfg.Risk.MaxDailyDrawdownPct = decimal.NewFromInt(5)
	}
	if cfg.Risk.MinLiquidationBufferPct.IsZero() {
		// Raised 2 -> 5 (2026-09-01, real-money wiring): with MaxLeverage now capped at 10x, the
		// conservative 100/leverage distance-to-liquidation estimate is always ~10%, so a 5% floor
		// never blocks a real order at this leverage — it's a final backstop against total-margin
		// liquidation, independent of and behind the 15% SL cap (conductor.Clamps.MaxLossPct) that
		// should always close a position long before this point is reached. Chose the more
		// conservative (larger) value per explicit operator instruction, same "don't trust one
		// layer alone" reasoning as the SL-cap incident (CLAUDE.md §23). Was 2 when MaxLeverage was
		// 100 (chosen specifically to keep near-100x leverage reachable, no longer applicable).
		cfg.Risk.MinLiquidationBufferPct = decimal.NewFromInt(5)
	}

	if cfg.API.Addr == "" {
		cfg.API.Addr = envOr("API_ADDR", "127.0.0.1:8090")
	}
	if cfg.API.ProcessMgr == "" {
		cfg.API.ProcessMgr = envOr("API_PROCESS_MANAGER", "systemd")
	}
	if len(cfg.API.Units) == 0 {
		// docker-compose's default container naming ("<project>-<service>-1"); override via
		// api.units in config.yaml or API_UNITS if the actual deployment names differ.
		cfg.API.Units = []string{"okxbot-rl-service-1"}
	}

	cfg.Optimizer.URL = envOr("OPTIMIZER_SERVICE_URL", "http://localhost:8001")
	if cfg.Optimizer.Addr == "" {
		cfg.Optimizer.Addr = envOr("OPTIMIZER_ADDR", "0.0.0.0:8091")
	}
	if cfg.Optimizer.ScheduleInterval == "" {
		cfg.Optimizer.ScheduleInterval = "24h"
	}
	if cfg.Optimizer.RunDuration == "" {
		cfg.Optimizer.RunDuration = "4h"
	}
	if cfg.Optimizer.MinTradesPerCandidate == 0 {
		cfg.Optimizer.MinTradesPerCandidate = 15
	}
	if cfg.Optimizer.MinImprovementPct.IsZero() {
		cfg.Optimizer.MinImprovementPct = decimal.NewFromInt(5)
	}
	if cfg.Optimizer.MinWinRatePctFloor.IsZero() {
		cfg.Optimizer.MinWinRatePctFloor = decimal.NewFromInt(50)
	}
	if cfg.Optimizer.Bar == "" {
		cfg.Optimizer.Bar = "15m"
	}
	if cfg.Optimizer.CandleWindow == 0 {
		cfg.Optimizer.CandleWindow = 100
	}
	if cfg.Optimizer.BatchSize == 0 {
		cfg.Optimizer.BatchSize = 5
	}
	if cfg.Optimizer.TrialTTLBufferSec == 0 {
		cfg.Optimizer.TrialTTLBufferSec = 3600
	}
	if len(cfg.Optimizer.DefaultKinds) == 0 {
		cfg.Optimizer.DefaultKinds = []string{"rsi_sma"}
	}
	if len(cfg.Optimizer.Targets) == 0 {
		for _, instID := range cfg.Trading.InstIDs {
			for _, kind := range cfg.Optimizer.DefaultKinds {
				cfg.Optimizer.Targets = append(cfg.Optimizer.Targets, OptimizerTarget{InstID: instID, Kind: kind})
			}
		}
	}

	cfg.Tester.URL = envOr("TESTER_SERVICE_URL", "http://localhost:8092")
	cfg.PaperTrading.URL = envOr("PAPER_TRADER_SERVICE_URL", "http://localhost:8093")
	cfg.Trading.URL = envOr("TRADER_SERVICE_URL", "http://localhost:8095")
	cfg.Gateway.URL = envOr("OKX_GATEWAY_URL", "http://localhost:8094")
	if cfg.Gateway.Addr == "" {
		cfg.Gateway.Addr = envOr("GATEWAY_ADDR", "0.0.0.0:8094")
	}
	if cfg.FillTimeout.OrderFillTimeoutSec == 0 {
		// 60s, per explicit operator decision (CLAUDE.md §27.5) — cancel-and-wait-for-next-signal,
		// never a synthetic retry at a new price.
		cfg.FillTimeout.OrderFillTimeoutSec = 60
	}
	if cfg.Tester.Addr == "" {
		cfg.Tester.Addr = envOr("TESTER_ADDR", "0.0.0.0:8092")
	}
	if cfg.Tester.Bar == "" {
		cfg.Tester.Bar = "5m"
	}
	if len(cfg.Tester.InstIDs) == 0 {
		cfg.Tester.InstIDs = cfg.Trading.InstIDs
	}
	if cfg.Tester.NotionalUSD.IsZero() {
		cfg.Tester.NotionalUSD = decimal.NewFromInt(10)
	}
	if cfg.Tester.Leverage.IsZero() {
		cfg.Tester.Leverage = decimal.NewFromInt(10)
	}
	if cfg.Tester.CandleWindow == 0 {
		cfg.Tester.CandleWindow = 300
	}
	if cfg.Tester.MaxOpenDuration == 0 {
		cfg.Tester.MaxOpenDuration = 6 * time.Hour
	}
	if cfg.Tester.Optimize.CheckInterval == 0 {
		cfg.Tester.Optimize.CheckInterval = time.Hour
	}
	// MaxLossPct defaults to a hard 15% cap (2026-08-31 request: "no limit with any leverage
	// should go down more than 15%") — unlike PaperTrading.RLClamps' other fields, this one is
	// not opt-in-only-if-configured, since an unbounded loss at high leverage is exactly the
	// failure mode it exists to prevent.
	if cfg.PaperTrading.RLClamps.MaxLossPct.IsZero() {
		cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	}
	if cfg.Tester.RLClamps.MaxLossPct.IsZero() {
		cfg.Tester.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	}
	if cfg.Optimizer.MaxLossPct.IsZero() {
		cfg.Optimizer.MaxLossPct = decimal.NewFromFloat(0.15)
	}

	return cfg, nil
}

// ValidatePaperTradingBars confirms every bar in PaperTrading.Bars is also present in
// Ingestion.Bars — cmd/paper-trader consumes a Redis stream per bar that only exists if
// cmd/ingestor is actually publishing to it, so a bar outside that set would silently receive no
// data rather than fail loudly.
func (c *Config) ValidatePaperTradingBars() error {
	ingested := make(map[string]bool, len(c.Ingestion.Bars))
	for _, bar := range c.Ingestion.Bars {
		ingested[bar] = true
	}
	for _, bar := range c.PaperTrading.Bars {
		if !ingested[bar] {
			return fmt.Errorf("paper_trading.bars contains %q, which is not in ingestion.bars — the ingestor isn't publishing that timeframe", bar)
		}
	}

	if b := c.PaperTrading.RLDecisionBar; b != "" {
		found := false
		for _, bar := range c.PaperTrading.Bars {
			if bar == b {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("paper_trading.rl_decision_bar is %q, which is not in paper_trading.bars — "+
				"the RL decision context must be a timeframe this process actually maintains a candle window for", b)
		}
	}

	return validateBarNames(append(append([]string{}, c.Ingestion.Bars...), c.PaperTrading.Bars...))
}

// okxBarNames are the candle timeframes OKX's WS channels accept, in OKX's exact casing. The
// casing is not cosmetic: the ingestor subscribes to "candle"+bar literally, so "1h" instead of
// "1H" silently subscribes to a channel that pushes nothing — the pipeline looks healthy while
// that timeframe never produces a candle. Rejecting it at startup turns a silent data gap into an
// immediate, obvious failure.
var okxBarNames = map[string]bool{
	"1m": true, "3m": true, "5m": true, "15m": true, "30m": true,
	"1H": true, "2H": true, "4H": true, "6H": true, "12H": true,
	"1D": true, "2D": true, "3D": true, "1W": true, "1M": true,
}

func validateBarNames(bars []string) error {
	for _, bar := range bars {
		if okxBarNames[bar] {
			continue
		}
		// Point at the exact fix when it's only a casing mistake, since that's the likely error.
		for name := range okxBarNames {
			if strings.EqualFold(name, bar) {
				return fmt.Errorf("bar %q has the wrong case for OKX's channel names — use %q "+
					"(OKX capitalizes hours/days/weeks: 1H, 4H, 1D; minutes stay lowercase: 5m, 15m)", bar, name)
			}
		}
		return fmt.Errorf("bar %q is not an OKX candle timeframe", bar)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
