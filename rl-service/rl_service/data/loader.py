"""Load historical candle data from CSV and prepare it for the trading environment."""
from __future__ import annotations

import pandas as pd

from rl_service.data.features import FEATURE_COLUMNS, add_features


def load_candles(csv_path: str) -> pd.DataFrame:
    df = pd.read_csv(csv_path)
    required = {"ts", "open", "high", "low", "close", "vol"}
    missing = required - set(df.columns)
    if missing:
        raise ValueError(f"candles csv {csv_path} missing required columns: {missing}")
    return df.sort_values("ts").reset_index(drop=True)


def load_features(csv_path: str) -> pd.DataFrame:
    """Load candles and append engineered feature columns (see FEATURE_COLUMNS)."""
    df = load_candles(csv_path)
    return add_features(df)


__all__ = ["load_candles", "load_features", "FEATURE_COLUMNS"]
