"""Read access to the shared Postgres database go-engine writes to (CLAUDE.md §15.8).

Deliberately read-only and narrow: rl_service never writes to these tables (Go owns writes), and
only queries the two tables the warm-start replay env and continued-training mode actually need —
`candles` (real OHLCV, written by PaperTrader since Phase 1) and `paper_orders` (closed trades plus
the decision-time observation logged in `features_json` as of the schema-v3 change).
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import psycopg2


def dsn_from_env() -> str:
    """Mirrors go-engine/internal/config.Load's POSTGRES_DSN convention/default."""
    return os.environ.get(
        "POSTGRES_DSN", "postgres://okxbot:okxbot@localhost:5432/okxbot"
    )


@dataclass
class CandleRow:
    inst_id: str
    bar: str
    ts: datetime
    open: float
    high: float
    low: float
    close: float
    volume: float


@dataclass
class PaperOrderRow:
    id: int
    inst_id: str
    strategy_id: int | None
    side: str
    entry_px: float
    sl_px: float | None
    tp_px: float | None
    opened_at: datetime
    closed_at: datetime | None
    close_reason: str | None
    realized_pnl: float | None
    variant: str
    parent_order_id: int | None
    # The decision-time domain.Observation this order was opened with, as logged by
    # PaperTrader.evaluateStrategies (Go) — None for orders opened before that change shipped.
    features_json: dict[str, Any] | None


def fetch_candles(conn, inst_id: str, bar: str, since: datetime | None = None) -> list[CandleRow]:
    """Oldest-first, matching the ordering strategies/the replay env expect."""
    query = "SELECT inst_id, bar, ts, open, high, low, close, volume FROM candles WHERE inst_id = %s AND bar = %s"
    params: list[Any] = [inst_id, bar]
    if since is not None:
        query += " AND ts >= %s"
        params.append(since)
    query += " ORDER BY ts ASC"

    with conn.cursor() as cur:
        cur.execute(query, params)
        return [
            CandleRow(inst_id=r[0], bar=r[1], ts=r[2], open=r[3], high=r[4], low=r[5], close=r[6], volume=r[7])
            for r in cur.fetchall()
        ]


def fetch_paper_orders(conn, inst_id: str | None = None, variant: str = "baseline") -> list[PaperOrderRow]:
    """Oldest-first. variant defaults to 'baseline' since forks (CLAUDE.md §15.4) are tracking-only
    and must never be double-counted as training data alongside their parent."""
    query = """
        SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, opened_at, closed_at,
               close_reason, realized_pnl, variant, parent_order_id, features_json
        FROM paper_orders
        WHERE variant = %s
    """
    params: list[Any] = [variant]
    if inst_id:
        query += " AND inst_id = %s"
        params.append(inst_id)
    query += " ORDER BY opened_at ASC"

    with conn.cursor() as cur:
        cur.execute(query, params)
        return [
            PaperOrderRow(
                id=r[0], inst_id=r[1], strategy_id=r[2], side=r[3], entry_px=r[4], sl_px=r[5],
                tp_px=r[6], opened_at=r[7], closed_at=r[8], close_reason=r[9], realized_pnl=r[10],
                variant=r[11], parent_order_id=r[12], features_json=r[13],
            )
            for r in cur.fetchall()
        ]


def connect(dsn: str | None = None):
    return psycopg2.connect(dsn or dsn_from_env())
