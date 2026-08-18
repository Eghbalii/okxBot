"""Feature engineering for the OKX futures trading environment.

All indicators are computed with simple pandas/numpy operations to avoid pulling in a heavy
TA-lib dependency. Feel free to swap in `ta`/`pandas-ta` later if richer indicators are needed.
"""
from __future__ import annotations

import numpy as np
import pandas as pd


def add_features(df: pd.DataFrame) -> pd.DataFrame:
    """Given OHLCV columns (open, high, low, close, vol), append engineered feature columns.

    Returned dataframe has NaNs from rolling windows dropped, so the index is reset.
    """
    out = df.copy()

    out["ret_1"] = out["close"].pct_change()
    out["log_ret_1"] = np.log(out["close"]).diff()

    for window in (5, 10, 20):
        out[f"sma_{window}"] = out["close"].rolling(window).mean()
        out[f"sma_ratio_{window}"] = out["close"] / out[f"sma_{window}"] - 1.0
        out[f"vol_{window}"] = out["log_ret_1"].rolling(window).std()

    out["rsi_14"] = _rsi(out["close"], period=14)

    vol_mean = out["vol"].rolling(20).mean()
    out["vol_ratio_20"] = out["vol"] / vol_mean.replace(0, np.nan)

    out = out.dropna().reset_index(drop=True)
    return out


def _rsi(close: pd.Series, period: int = 14) -> pd.Series:
    delta = close.diff()
    gain = delta.clip(lower=0)
    loss = -delta.clip(upper=0)
    avg_gain = gain.ewm(alpha=1 / period, min_periods=period).mean()
    avg_loss = loss.ewm(alpha=1 / period, min_periods=period).mean()
    rs = avg_gain / avg_loss.replace(0, np.nan)
    rsi = 100 - (100 / (1 + rs))
    return rsi.fillna(50.0)


FEATURE_COLUMNS = [
    "ret_1",
    "log_ret_1",
    "sma_ratio_5",
    "vol_5",
    "sma_ratio_10",
    "vol_10",
    "sma_ratio_20",
    "vol_20",
    "rsi_14",
    "vol_ratio_20",
]
