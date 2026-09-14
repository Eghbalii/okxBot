"""Continuous learning from live trading outcomes (CLAUDE.md §15.11).

The problem this solves: a decision and its consequence are separated by the whole life of a trade.
The model sizes a position at 10:00; whether that was right is only known at 14:00 when the trade
closes. So a decision cannot be scored when it is made — it has to be held until its outcome
arrives.

That is what `Learner` does. Every `/predict` call that produces a real decision is remembered as a
*pending* decision keyed by order id; the terminal (`closed_*`) call for that order supplies the
realized PnL, which becomes the reward, and the completed experience is pushed into SAC's replay
buffer. Gradient steps run from there.

Why SAC rather than PPO: PPO is on-policy and learns only from a fresh batch of its own current
policy's actions, discarding each batch after one update. At tens of trades per day that batch takes
weeks to fill, which makes continuous learning impossible. SAC keeps every experience in a replay
buffer and reuses it, so it learns from a trickle.

Thread-safety: FastAPI serves requests from a thread pool, so several calls can touch this
concurrently. All mutable state is guarded by one lock, and gradient steps run inside it — a
gradient step and an inference call reading the same weights concurrently would otherwise race.
"""
from __future__ import annotations

import logging
import os
import threading
import time
from dataclasses import dataclass, field
from typing import Optional

import numpy as np

logger = logging.getLogger("rl_service.learner")

# A pending decision older than this is discarded. Its trade either never closed or its terminal
# call was lost, and holding it forever would leak memory in a long-running service. Generous
# because a position can legitimately stay open for a long time.
PENDING_TTL_SECONDS = 7 * 24 * 3600

# Beyond this many pending decisions the oldest are dropped, whatever their age — a bound against
# a caller that opens decisions and never closes them.
MAX_PENDING = 10_000


@dataclass
class _Pending:
    """One decision awaiting its outcome."""

    obs_vec: np.ndarray
    action: np.ndarray
    created_at: float = field(default_factory=time.monotonic)


class Learner:
    """Holds the replay buffer and turns closed trades into gradient steps.

    Deliberately owns no HTTP concerns: the API layer decides *when* to record and complete, this
    decides what that means for the model.
    """

    def __init__(
        self,
        model,
        *,
        learning_starts: int = 100,
        gradient_steps: int = 4,
        snapshot_every: int = 25,
        model_path: str = "",
        buffer_path: str = "",
    ):
        self.model = model
        self.learning_starts = learning_starts
        self.gradient_steps = gradient_steps
        self.snapshot_every = snapshot_every
        self.model_path = model_path
        self.buffer_path = buffer_path

        self._lock = threading.Lock()
        self._pending: dict[int, _Pending] = {}
        self.completed = 0
        self.updates = 0
        self.last_reward: Optional[float] = None

        # SB3 sets up a logger inside learn(), which this service never calls — it drives gradient
        # steps directly instead. Without one, train() raises on its first metric write, and the
        # error handler below would swallow it: the service would look healthy while silently never
        # learning anything. Attach one up front so that cannot happen.
        if getattr(self.model, "_logger", None) is None:
            from stable_baselines3.common.logger import configure

            self.model.set_logger(configure(folder=None, format_strings=[]))

    # --- recording ------------------------------------------------------------------------

    def record(self, order_id: int, obs_vec: np.ndarray, action: np.ndarray) -> None:
        """Remembers a decision until its outcome arrives.

        order_id is the join key; a decision with no order id cannot be scored later, so the caller
        is expected to skip those rather than have this invent an id.
        """
        if order_id <= 0:
            return
        with self._lock:
            self._pending[order_id] = _Pending(
                obs_vec=np.asarray(obs_vec, dtype=np.float32).reshape(-1),
                action=np.asarray(action, dtype=np.float32).reshape(-1),
            )
            self._evict_locked()

    def complete(self, order_id: int, reward: float, final_obs_vec: np.ndarray) -> bool:
        """Pairs a closed trade's reward with the decision that produced it and learns from it.

        Returns whether a pending decision was actually found — a false here means the terminal call
        arrived for something never recorded (a restart lost it, or the decision predates learning
        being enabled), which is expected rather than an error.
        """
        with self._lock:
            pending = self._pending.pop(order_id, None)
            if pending is None:
                return False

            self.completed += 1
            self.last_reward = reward

            # The trade is over, so this transition is terminal: there is no next decision flowing
            # from it, and bootstrapping a future value off the final observation would credit the
            # policy for a position that no longer exists.
            self.model.replay_buffer.add(
                obs=pending.obs_vec.reshape(1, -1),
                next_obs=np.asarray(final_obs_vec, dtype=np.float32).reshape(1, -1),
                action=pending.action.reshape(1, -1),
                reward=np.array([reward], dtype=np.float32),
                done=np.array([True]),
                infos=[{}],
            )

            self._maybe_train_locked()
            self._maybe_snapshot_locked()
            return True

    # --- training -------------------------------------------------------------------------

    def _maybe_train_locked(self) -> None:
        if self.model.replay_buffer.size() < self.learning_starts:
            return
        try:
            # batch_size is the model's own configured size; gradient_steps reuses stored
            # experience several times per new trade, which is the whole point of off-policy.
            self.model.train(gradient_steps=self.gradient_steps, batch_size=self.model.batch_size)
            self.updates += 1
        except Exception:
            # A failed gradient step must never take the inference path down with it: the service's
            # first duty is answering /predict, and a frozen-but-serving model beats a dead one.
            logger.exception("gradient step failed; continuing to serve current weights")

    def _maybe_snapshot_locked(self) -> None:
        if self.snapshot_every <= 0 or self.completed % self.snapshot_every != 0:
            return
        try:
            self.snapshot_locked()
        except Exception:
            logger.exception("snapshot failed; learning continues in memory")

    def snapshot_locked(self) -> None:
        """Writes weights AND replay buffer.

        Both, always: weights alone would come back on restart having silently forgotten every
        experience collected since the last snapshot, which for a service whose whole purpose is
        accumulating scarce experience is worse than not snapshotting at all.
        """
        if not self.model_path:
            return
        os.makedirs(os.path.dirname(self.model_path) or ".", exist_ok=True)
        self.model.save(self.model_path)
        if self.buffer_path:
            self.model.save_replay_buffer(self.buffer_path)
        logger.info(
            "snapshot written: %s (+buffer), %d completed trades, %d updates",
            self.model_path, self.completed, self.updates,
        )

    def snapshot(self) -> None:
        with self._lock:
            self.snapshot_locked()

    # --- housekeeping ---------------------------------------------------------------------

    def _evict_locked(self) -> None:
        """Drops decisions whose outcome will never arrive, so a long-running service can't leak."""
        now = time.monotonic()
        stale = [k for k, p in self._pending.items() if now - p.created_at > PENDING_TTL_SECONDS]
        for k in stale:
            del self._pending[k]

        if len(self._pending) > MAX_PENDING:
            oldest = sorted(self._pending.items(), key=lambda kv: kv[1].created_at)
            for k, _ in oldest[: len(self._pending) - MAX_PENDING]:
                del self._pending[k]

    def stats(self) -> dict:
        with self._lock:
            return {
                "pending": len(self._pending),
                "buffer_size": int(self.model.replay_buffer.size()),
                "completed_trades": self.completed,
                "updates": self.updates,
                "last_reward": self.last_reward,
            }


def reward_from_outcome(
    realized_pnl_usd: float,
    account_initial_usd: float,
    position_size_usd: float = 0.0,
) -> float:
    """Turns a closed trade's realized PnL into the reward the policy learns from.

    Normalized by the POSITION's own capital rather than the account's, when the size is known
    (changed 2026-09-14). Both are scale-free, which was the original requirement (CLAUDE.md
    §15.11: a $5 win means something different on a $100 account than on a $10,000 one), but
    dividing by the account makes the reward depend on a number the trade had nothing to do with.

    Measured consequence: with a $2,600 account and typical PnL near $0.10, rewards landed at 1e-4
    to 1e-3 — indistinguishable from zero to SAC. Worse, raising the account cap from $200 to
    $2,600 that same day shrank every reward thirteenfold without a single trade changing, so the
    policy was told its results had collapsed when nothing about them had.

    Dividing by position size instead gives return-on-capital: a trade that makes 5% of what it
    risked scores 0.05 whether the account is $200 or $2,600, and the figure means the same thing
    across every instrument and position size.

    Falls back to the account denominator when size is unknown (0), so historical rows and the
    pretrain path keep working rather than silently scoring zero.
    """
    pnl = float(realized_pnl_usd)
    size = float(position_size_usd or 0.0)
    if size > 0:
        return pnl / size
    if not account_initial_usd:
        return 0.0
    return pnl / float(account_initial_usd)
