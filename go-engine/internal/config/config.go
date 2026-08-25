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

	PaperTrading struct {
		NotionalUSD   decimal.Decimal `yaml:"notional_usd"`
		MaxOpenOrders int             `yaml:"max_open_orders"`
		Bars          []string        `yaml:"bars"` // candle timeframes to subscribe to, e.g. ["1m", "15m", "1h"]
		CandleLimit   int             `yaml:"candle_limit"`
	} `yaml:"paper_trading"`

	Risk struct {
		MaxLeverage             decimal.Decimal `yaml:"max_leverage"`
		MaxPositionNotionalUSD  decimal.Decimal `yaml:"max_position_notional_usd"`
		MaxDailyDrawdownPct     decimal.Decimal `yaml:"max_daily_drawdown_pct"`
		MinLiquidationBufferPct decimal.Decimal `yaml:"min_liquidation_buffer_pct"`
	} `yaml:"risk"`
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
	if len(cfg.PaperTrading.Bars) == 0 {
		cfg.PaperTrading.Bars = []string{"1m"}
	}
	if cfg.PaperTrading.CandleLimit == 0 {
		cfg.PaperTrading.CandleLimit = 100
	}
	if cfg.Risk.MaxLeverage.IsZero() {
		cfg.Risk.MaxLeverage = decimal.NewFromInt(5)
	}
	if cfg.Risk.MaxPositionNotionalUSD.IsZero() {
		cfg.Risk.MaxPositionNotionalUSD = decimal.NewFromInt(1000)
	}
	if cfg.Risk.MaxDailyDrawdownPct.IsZero() {
		cfg.Risk.MaxDailyDrawdownPct = decimal.NewFromInt(5)
	}
	if cfg.Risk.MinLiquidationBufferPct.IsZero() {
		cfg.Risk.MinLiquidationBufferPct = decimal.NewFromInt(15)
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
