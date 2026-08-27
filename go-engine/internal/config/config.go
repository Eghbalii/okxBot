// Package config loads runtime configuration from environment variables and a YAML file.
package config

import (
	"fmt"
	"os"

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

	Redis struct {
		Addr string `yaml:"-"`
	} `yaml:"redis"`

	Postgres struct {
		DSN string `yaml:"-"`
	} `yaml:"postgres"`

	RLService struct {
		URL string `yaml:"-"`
	} `yaml:"rl_service"`

	Trading struct {
		InstIDs         []string        `yaml:"inst_ids"`
		PollIntervalSec int             `yaml:"poll_interval_sec"`
		TdMode          string          `yaml:"td_mode"`       // "cross" or "isolated"
		PosMode         string          `yaml:"pos_mode"`      // "net" or "long_short" (hedge mode)
		MinOrderUSD     decimal.Decimal `yaml:"min_order_usd"` // skip rebalancing orders smaller than this
	} `yaml:"trading"`

	// Ingestion controls cmd/ingestor: the always-on, broad set of candle timeframes it collects
	// from OKX and publishes to Redis, independent of what any given paper-trading/strategy test
	// run actually evaluates (see PaperTrading.Bars).
	Ingestion struct {
		Bars []string `yaml:"bars"`
	} `yaml:"ingestion"`

	PaperTrading struct {
		NotionalUSD   decimal.Decimal `yaml:"notional_usd"`
		MaxOpenOrders int             `yaml:"max_open_orders"`
		// Bars is the subset of Ingestion.Bars that cmd/paper-trader actually maintains candle
		// windows for and evaluates strategies against — changeable per test run without
		// touching the always-on ingestor. Must be a subset of Ingestion.Bars (see
		// ValidatePaperTradingBars); a bar not being collected by the ingestor has no Redis
		// stream to consume from.
		Bars        []string `yaml:"bars"`
		CandleLimit int      `yaml:"candle_limit"`
		// TokenBudgetUSD is each token's fixed paper-mode sub-budget (CLAUDE.md §15.6/§15.7) — not
		// learned/RL-allocated in Phase A, just a config value each PaperTrader instance is given.
		TokenBudgetUSD decimal.Decimal `yaml:"token_budget_usd"`
		// RLSLTPAdjust enables the RL-driven in-trade SL/TP adjustment pass (CLAUDE.md §15.4). Off
		// by default: PaperTrader runs exactly as it did before §15 wherever this is false, since a
		// meaningful decision here requires a trained (or at least deliberately no-op) RL model to
		// be reachable at RLService.URL.
		RLSLTPAdjust bool `yaml:"rl_sltp_adjust"`
	} `yaml:"paper_trading"`

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
	} `yaml:"optimizer"`
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
	cfg.Postgres.DSN = envOr("POSTGRES_DSN", "postgres://okxbot:okxbot@localhost:5432/okxbot")
	cfg.RLService.URL = envOr("RL_SERVICE_URL", "http://localhost:8000")

	if len(cfg.Trading.InstIDs) == 0 {
		cfg.Trading.InstIDs = []string{"BTC-USDT-SWAP"}
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
		cfg.PaperTrading.NotionalUSD = decimal.NewFromInt(100)
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
	if cfg.PaperTrading.TokenBudgetUSD.IsZero() {
		// CLAUDE.md §15.6: your stated $10/token against a <$50 total budget.
		cfg.PaperTrading.TokenBudgetUSD = decimal.NewFromInt(10)
	}
	if cfg.Risk.MaxLeverage.IsZero() {
		// CLAUDE.md §15.4: raised from the earlier 5x default toward the RL agent's target 10x-100x
		// range. MinLiquidationBufferPct below is what actually bounds how much of this range is
		// reachable in practice (risk.Manager.Approve rejects any leverage whose estimated
		// liquidation buffer falls below that floor) — the two limits do separate jobs: this is a
		// ceiling, MinLiquidationBufferPct is the real liquidation-distance safety check.
		cfg.Risk.MaxLeverage = decimal.NewFromInt(100)
	}
	if cfg.Risk.MaxPositionNotionalUSD.IsZero() {
		cfg.Risk.MaxPositionNotionalUSD = decimal.NewFromInt(1000)
	}
	if cfg.Risk.MaxDailyDrawdownPct.IsZero() {
		cfg.Risk.MaxDailyDrawdownPct = decimal.NewFromInt(5)
	}
	if cfg.Risk.MinLiquidationBufferPct.IsZero() {
		// Lowered from 15% so leverage toward the top of the new 100x ceiling is reachable at all
		// (the conservative 100/leverage buffer estimate in usecase/trade.go gives ~2% buffer at
		// 50x, ~1% at 100x) — an explicit, deliberate tradeoff of real liquidation-distance safety
		// margin for usable leverage range, made because the RL agent's leverage choice is a
		// learned decision under this cap, not because thin buffers are risk-free. Revisit this
		// value carefully, especially before any real-money wiring (§14) — a 2% floor means a 2%
		// adverse move liquidates the position outright.
		cfg.Risk.MinLiquidationBufferPct = decimal.NewFromInt(2)
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
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
