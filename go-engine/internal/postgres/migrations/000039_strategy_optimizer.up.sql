-- Real strategy-parameter backtest/optimize pipeline (2026-09-27 request), replacing the abandoned
-- live-trial design (CLAUDE.md §21/§33.5: cmd/strategy-optimizer/cmd/strategy-tester, both removed
-- entirely rather than patched — judged "کاملا بی استفاده و بی فایده", completely useless).
--
-- Deliberately does NOT reimplement backtesting: candidate validation calls the EXISTING
-- internal/backtest.Runner (docs/RL_V8_PLAN.md's warm-start replay engine), which already replays
-- real strategy.Strategy code against real stored candles with statistical significance testing
-- against a null baseline (coin_flip). This table only owns the CANDIDATE LIFECYCLE Optuna drives
-- through that existing tool: proposed -> backtested -> approved/rejected -> live in paper trading.
--
-- Per (kind, inst_id, bar, exchange, risk_profile) lineage, not per-kind-only — the operator's
-- explicit "a strategy that only works well on one token is a real win, we are not chasing one
-- universal strategy." exchange is a free-text column, never hardcoded to "okx"/"mexc" in a CHECK
-- constraint — the operator's explicit "هرچی که میسازیم باید برای تمام صرافی ها کار بکنه", so a
-- 20th exchange added later needs no migration here.
CREATE TABLE IF NOT EXISTS strategy_candidates (
    id BIGSERIAL PRIMARY KEY,
    -- Matches strategy.Factories' kind key. NOT a FK — a candidate must survive independent of any
    -- production `strategies` row (same reasoning as the old tester_strategy_versions, §16.3).
    kind TEXT NOT NULL,
    inst_id TEXT NOT NULL,
    bar TEXT NOT NULL,
    exchange TEXT NOT NULL,
    risk_profile TEXT NOT NULL CHECK (risk_profile IN ('low', 'high')),

    config JSONB NOT NULL DEFAULT '{}',
    parent_candidate_id BIGINT REFERENCES strategy_candidates(id),

    -- The three counters behind the display name
    -- (kind_TOKEN_R{leverage}_G{generation}_B{backtest_updates}_P{paper_updates}, computed in Go
    -- from these three ints, never stored as a string — renaming the display convention later
    -- needs no backfill).
    --   generation: which copy-from-origin lineage this is (G1 = the first copy made from the
    --     kind's unconfigured defaults, INCLUDING each of the 12 pre-existing "_v2" kinds, treated
    --     as an already-existing G1 seed per explicit operator instruction, 2026-09-27).
    --   backtest_updates: how many times the optimizer replaced this lineage's params while still
    --     in the backtest phase.
    --   paper_updates: how many times the optimizer updated this lineage's params AFTER promotion
    --     into paper trading.
    generation INT NOT NULL DEFAULT 1,
    backtest_updates INT NOT NULL DEFAULT 0,
    paper_updates INT NOT NULL DEFAULT 0,

    -- 'proposed': freshly suggested by Optuna, backtest not yet run.
    -- 'backtesting': a backtest.Runner pass is currently in flight for this candidate.
    -- 'backtest_rejected': ran, failed validation — terminal, kept for audit/history only.
    -- 'backtest_passed': ran, passed validation, awaiting promotion into strategy_assignments.
    -- 'paper_active': promoted — has a live strategies/strategy_assignments row, the one version
    --   of this lineage actually trading in paper mode.
    -- 'paper_replaced': was paper_active, a later candidate for the same lineage beat it.
    status TEXT NOT NULL DEFAULT 'proposed'
        CHECK (status IN ('proposed', 'backtesting', 'backtest_rejected', 'backtest_passed', 'paper_active', 'paper_replaced')),

    -- Optuna sidecar's opaque trial number this candidate was proposed from (optimizer-service's
    -- study.ask() return value) — NULL for a manually-seeded G1 row (origin defaults, or one of the
    -- 12 existing "_v2" kinds copied in as a G1 seed).
    trial_id INT,
    source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('origin', 'manual', 'optimizer')),

    -- Backtest result, read straight from internal/backtest.Result.ByStrategy[kind] +
    -- .Significance — stored as columns (not a JSONB blob) so the validation-threshold comparison
    -- and the panel can query them directly.
    backtest_trade_count INT,
    backtest_win_rate_pct NUMERIC,
    backtest_realized_pnl NUMERIC,
    backtest_resets INT,          -- Result.Resets for the run this candidate was scored in
    backtest_significance_t NUMERIC, -- Significance.T vs. coin_flip, NULL if not computed
    backtest_from TIMESTAMPTZ,
    backtest_to TIMESTAMPTZ,
    backtest_ran_at TIMESTAMPTZ,
    -- Free-text explanation of why backtest_rejected — an operator reading the panel should never
    -- have to re-derive why something was rejected.
    backtest_rejection_reason TEXT,

    -- Set the moment this candidate is promoted into strategies/strategy_assignments (status
    -- transitions to 'paper_active') — the panel's stats views filter paper-side stats to "since
    -- this timestamp" so a promoted candidate's live numbers can never be contaminated by whatever
    -- the SAME strategies-table row logged before this pipeline started managing it.
    promoted_at TIMESTAMPTZ,
    -- The production strategies.id this candidate was promoted into, once promoted. Nullable, no
    -- FK (mirrors this table's own no-FK-to-strategies reasoning).
    strategy_id BIGINT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_strategy_candidates_lineage
    ON strategy_candidates (kind, inst_id, bar, exchange, risk_profile);
CREATE INDEX IF NOT EXISTS idx_strategy_candidates_status
    ON strategy_candidates (status);
-- At most one 'paper_active' candidate per lineage — enforced structurally rather than only by
-- application code remembering to demote the old one first (the exact class of bug CLAUDE.md §21
-- documents for the old tester_strategy_versions, where application code disabling a sibling was
-- the ONLY thing enforcing single-active-version, and it disabled too much/too eagerly).
CREATE UNIQUE INDEX IF NOT EXISTS idx_strategy_candidates_one_active_per_lineage
    ON strategy_candidates (kind, inst_id, bar, exchange, risk_profile)
    WHERE status = 'paper_active';

-- One row per risk profile: the validation thresholds a backtest result must clear to be
-- promoted, panel-editable per explicit operator request ("قابل تنظیمش کن که یوزر اگر خواست بعدا
-- تغییرش بده"). Combines several dimensions deliberately ("ترکیبی باشه نه فقط تمرکز روی یک چیز"),
-- matching what internal/backtest.Result already reports per strategy.
CREATE TABLE IF NOT EXISTS strategy_optimizer_config (
    risk_profile TEXT PRIMARY KEY CHECK (risk_profile IN ('low', 'high')),
    min_trades INT NOT NULL DEFAULT 30,
    min_win_rate_pct NUMERIC NOT NULL DEFAULT 45,
    min_realized_pnl NUMERIC NOT NULL DEFAULT 0,
    -- Minimum |t| vs. the coin_flip null baseline (internal/backtest.Significance.T) — the
    -- statistical-significance dimension RL_V8_PLAN.md's own screening established as necessary:
    -- a raw PnL/win-rate ranking without this is "one statistical cloud" that looks like a ranking
    -- and carries no information (measured directly: 76,245 real trades, every gap under |t|=2).
    min_significance_t NUMERIC NOT NULL DEFAULT 2,
    -- Max acceptable account resets (§15.7) during the backtest window — a candidate that drains
    -- the simulated account repeatedly describes a strategy mix that loses money regardless of its
    -- other numbers.
    max_resets INT NOT NULL DEFAULT 2,
    -- Wall-clock lookback window a backtest run replays, e.g. '30d' — parsed Go-side as a
    -- time.Duration-compatible string, same convention config.yaml already uses elsewhere.
    backtest_lookback TEXT NOT NULL DEFAULT '30d',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO strategy_optimizer_config (risk_profile) VALUES ('low'), ('high')
    ON CONFLICT (risk_profile) DO NOTHING;

-- One row per lineage: which strategy_candidates.id (if any) is the optimizer's current in-flight
-- backtest candidate — mirrors the old tester_optimizer_state's precedent (migration 000013,
-- removed) but scoped to a full lineage, not just a kind, since this pipeline is per-token.
CREATE TABLE IF NOT EXISTS strategy_optimizer_state (
    kind TEXT NOT NULL,
    inst_id TEXT NOT NULL,
    bar TEXT NOT NULL,
    exchange TEXT NOT NULL,
    risk_profile TEXT NOT NULL,
    candidate_id BIGINT REFERENCES strategy_candidates(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, inst_id, bar, exchange, risk_profile)
);
