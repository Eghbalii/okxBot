"""Fetch historical OKX candlestick data (public endpoint, no auth needed) for backtesting/training.

Usage:
    python -m rl_service.data.fetch_okx_history --inst-id BTC-USDT-SWAP --bar 1H --limit-bars 20000
"""
from __future__ import annotations

import argparse
import time

import pandas as pd
import requests

OKX_HISTORY_CANDLES_URL = "https://www.okx.com/api/v5/market/history-candles"

# OKX returns at most 100 candles per request, newest first; paginate backwards using `after`.
_PAGE_SIZE = 100


def fetch_candles(inst_id: str, bar: str, total_bars: int) -> pd.DataFrame:
    rows: list[list[str]] = []
    after: str | None = None

    while len(rows) < total_bars:
        params = {"instId": inst_id, "bar": bar, "limit": str(_PAGE_SIZE)}
        if after:
            params["after"] = after

        resp = requests.get(OKX_HISTORY_CANDLES_URL, params=params, timeout=10)
        resp.raise_for_status()
        payload = resp.json()
        if payload.get("code") != "0":
            raise RuntimeError(f"OKX API error: {payload}")

        data = payload["data"]
        if not data:
            break

        rows.extend(data)
        after = data[-1][0]  # oldest ts in this page -> paginate further back
        time.sleep(0.15)  # stay well under OKX's public rate limit

    columns = ["ts", "open", "high", "low", "close", "vol", "volCcy", "volCcyQuote", "confirm"]
    df = pd.DataFrame(rows, columns=columns[: len(rows[0])] if rows else columns)
    if df.empty:
        return df

    df["ts"] = pd.to_numeric(df["ts"])
    for col in ("open", "high", "low", "close", "vol"):
        df[col] = pd.to_numeric(df[col])
    df = df.sort_values("ts").drop_duplicates("ts").reset_index(drop=True)
    return df[["ts", "open", "high", "low", "close", "vol"]]


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inst-id", default="BTC-USDT-SWAP")
    parser.add_argument("--bar", default="1H")
    parser.add_argument("--limit-bars", type=int, default=20_000)
    parser.add_argument("--out", default=None)
    args = parser.parse_args()

    df = fetch_candles(args.inst_id, args.bar, args.limit_bars)
    out_path = args.out or f"data/{args.inst_id}_{args.bar}.csv"
    df.to_csv(out_path, index=False)
    print(f"Saved {len(df)} candles to {out_path}")


if __name__ == "__main__":
    main()
