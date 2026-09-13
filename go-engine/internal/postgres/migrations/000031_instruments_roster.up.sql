-- The tradeable-instrument roster, moved out of config.yaml into the database (2026-09-13).
--
-- Why: the token-discovery scan (internal/usecase.MarketScanner) finds new high-volume/trending
-- tokens on every supported exchange and must be able to put them to work without a human editing
-- YAML. Before this table, the roster was config.yaml's trading.inst_ids plus a hand-maintained
-- trading.symbol_map, resolved ONCE at each service's startup — so a scanned token had neither an
-- entry nor an exec instId, and could not reach the ingestor's WebSocket subscriptions at all.
--
-- The per-mode enable flags are three independent switches ON PURPOSE, not one "enabled" column:
-- a scanned token is admitted to data collection and paper trading immediately (that is how it
-- earns a track record) while staying OFF for real money until a person turns it on. Collapsing
-- them would make discovery and real-capital exposure the same decision, which is exactly the
-- decision that must stay manual.
--
-- exec_inst_id is the exchange's own wire-format instrument id (OKX's "BTC-USD_UM_XPERP-310404",
-- MEXC's "BTC_USDT") and replaces trading.symbol_map. It stays a stored column rather than being
-- derived: OKX's X-Perp ids embed a rolling expiry date that is not derivable from the symbol and
-- changes when OKX rolls the contract (CLAUDE.md §33.4), while MEXC's are plainly derivable — one
-- column covers both without the code having to know which case it is looking at.
CREATE TABLE IF NOT EXISTS instruments (
    id            BIGSERIAL PRIMARY KEY,
    -- The short internal symbol every other table, Kafka key and panel row already carries
    -- (CLAUDE.md §33.4). Unique per exchange, not globally: the same token legitimately trades on
    -- several exchanges, each with its own instrument id, volume and tick size.
    symbol        TEXT NOT NULL,
    exchange      TEXT NOT NULL,
    exec_inst_id  TEXT NOT NULL,
    -- The instType/product family the exec id lives under ("FUTURES" on OKX's X-Perp, "SWAP" on
    -- the classic perpetuals, "" where an exchange has no such concept).
    inst_type     TEXT NOT NULL DEFAULT '',

    enabled_ingest BOOLEAN NOT NULL DEFAULT TRUE,
    enabled_paper  BOOLEAN NOT NULL DEFAULT TRUE,
    -- FALSE by default, deliberately: see the header. Real-money exposure is opt-in per token.
    enabled_real   BOOLEAN NOT NULL DEFAULT FALSE,

    -- How this row came to exist, so a reader can always tell a scan's work from a person's
    -- (the same provenance lesson as migration 000030's auto_disabled_inst_ids).
    source        TEXT NOT NULL DEFAULT 'manual'
                  CHECK (source IN ('manual', 'scan', 'seed')),

    -- Ranking snapshot from the scan that admitted or last refreshed this row. Nullable because a
    -- manually-added or seeded row has never been scored.
    vol_24h_usd   NUMERIC,
    change_24h_pct NUMERIC,
    scan_score    NUMERIC,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exchange, symbol)
);

-- The ingestor/paper-trader read "every row enabled for me", which is the only access pattern.
CREATE INDEX IF NOT EXISTS instruments_ingest_idx ON instruments (exchange) WHERE enabled_ingest;

-- Every scanned candidate, whether or not it was admitted to the roster — the ranked market view
-- the Home page shows and sorts. Separate from instruments because most rows here will never be
-- traded: this is the whole market, that is the working set.
--
-- One row per (exchange, symbol), overwritten by each scan rather than appended. A scan runs a few
-- times a day and the panel only ever asks "what does the market look like now", so keeping history
-- would grow without bound to answer a question nobody is asking. Add a history table if a
-- "trending over the last week" view is ever wanted.
CREATE TABLE IF NOT EXISTS market_tokens (
    exchange       TEXT NOT NULL,
    symbol         TEXT NOT NULL,
    exec_inst_id   TEXT NOT NULL,
    last_px        NUMERIC NOT NULL,
    open_24h       NUMERIC NOT NULL,
    high_24h       NUMERIC NOT NULL,
    low_24h        NUMERIC NOT NULL,
    vol_24h_usd    NUMERIC NOT NULL,
    change_24h_pct NUMERIC NOT NULL,
    -- Intraday range as a percentage of price: a cheap volatility/opportunity proxy that, unlike
    -- change_24h_pct, does not cancel out on a token that moved hard in both directions.
    range_24h_pct  NUMERIC NOT NULL,
    -- The composite the scan ranks by (internal/usecase.ScoreToken). Stored so the panel sorts by
    -- the same number the scan admitted on, rather than recomputing it with drifting weights.
    score          NUMERIC NOT NULL,
    scanned_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exchange, symbol)
);

CREATE INDEX IF NOT EXISTS market_tokens_score_idx ON market_tokens (score DESC);
