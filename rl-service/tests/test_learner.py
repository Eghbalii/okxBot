"""Tests for continuous learning (CLAUDE.md §15.11).

The thing being verified: a decision and its consequence are separated by the whole life of a
trade, so the learner has to hold each decision until its outcome arrives and pair them by order
id. Getting that pairing wrong would train the model on the wrong trade's outcome, which no error
would ever surface.
"""
from __future__ import annotations

import numpy as np
import pytest
from stable_baselines3 import SAC

from rl_service.learner import MAX_PENDING, Learner, reward_from_outcome
from rl_service.obs import ACTION_DIM

OBS_DIM = 40


@pytest.fixture
def learner(tmp_path):
    import gymnasium as gym
    from gymnasium import spaces

    class Dummy(gym.Env):
        observation_space = spaces.Box(-np.inf, np.inf, (OBS_DIM,), np.float32)
        action_space = spaces.Box(-1, 1, (ACTION_DIM,), np.float32)

        def reset(self, *, seed=None, options=None):
            return np.zeros(OBS_DIM, np.float32), {}

        def step(self, a):
            return np.zeros(OBS_DIM, np.float32), 0.0, False, False, {}

    model = SAC("MlpPolicy", Dummy(), buffer_size=1000, batch_size=8, verbose=0)
    return Learner(
        model,
        learning_starts=4,
        gradient_steps=1,
        snapshot_every=0,  # snapshots tested separately
        model_path=str(tmp_path / "m.zip"),
        buffer_path=str(tmp_path / "b.pkl"),
    )


def _obs():
    return np.random.rand(OBS_DIM).astype(np.float32)


def _action():
    return np.random.rand(ACTION_DIM).astype(np.float32)


def test_decision_is_paired_with_its_own_outcome(learner):
    """Two trades open, then close in the OPPOSITE order.

    Rewards must follow order ids, not arrival order — otherwise a winning trade's reward would
    train the losing trade's decision, and nothing would ever report an error.
    """
    obs_a, act_a = _obs(), _action()
    obs_b, act_b = _obs(), _action()
    learner.record(order_id=1, obs_vec=obs_a, action=act_a)
    learner.record(order_id=2, obs_vec=obs_b, action=act_b)

    assert learner.complete(order_id=2, reward=0.5, final_obs_vec=_obs())
    assert learner.complete(order_id=1, reward=-0.2, final_obs_vec=_obs())

    assert learner.stats()["pending"] == 0
    assert learner.stats()["buffer_size"] == 2

    # The buffer must hold the ACTIONS that were recorded, in completion order: trade 2 closed
    # first, so its action is stored first. Pairing by arrival order instead of by id would put
    # trade 2's reward against trade 1's action, and nothing would report an error.
    stored = learner.model.replay_buffer.actions[:2].reshape(2, -1)
    assert np.allclose(stored[0], act_b, atol=1e-5), "first stored action must be trade 2's (closed first)"
    assert np.allclose(stored[1], act_a, atol=1e-5), "second stored action must be trade 1's"

    # And the rewards must line up with those same actions.
    rewards = learner.model.replay_buffer.rewards[:2].reshape(-1)
    assert rewards[0] == pytest.approx(0.5), "trade 2's reward must accompany trade 2's action"
    assert rewards[1] == pytest.approx(-0.2), "trade 1's reward must accompany trade 1's action"


def test_unknown_order_is_reported_not_raised(learner):
    """A terminal call for something never recorded is expected after a restart, not an error."""
    assert learner.complete(order_id=999, reward=1.0, final_obs_vec=_obs()) is False
    assert learner.stats()["buffer_size"] == 0


def test_decision_without_order_id_is_not_recorded(learner):
    """Without a join key the outcome could never be paired back, so it must not be held."""
    learner.record(order_id=0, obs_vec=_obs(), action=_action())
    assert learner.stats()["pending"] == 0


def test_no_training_until_learning_starts(learner):
    """Updating from a nearly-empty buffer just overfits the first few trades."""
    for i in range(3):  # learning_starts is 4
        learner.record(order_id=i + 1, obs_vec=_obs(), action=_action())
        learner.complete(order_id=i + 1, reward=0.1, final_obs_vec=_obs())
    assert learner.updates == 0

    learner.record(order_id=99, obs_vec=_obs(), action=_action())
    learner.complete(order_id=99, reward=0.1, final_obs_vec=_obs())
    assert learner.updates == 1, "training must begin once the buffer reaches learning_starts"


def test_transitions_are_terminal(learner):
    """A closed trade has no successor decision.

    Bootstrapping a future value off the final observation would credit the policy for a position
    that no longer exists.
    """
    learner.record(order_id=1, obs_vec=_obs(), action=_action())
    learner.complete(order_id=1, reward=0.3, final_obs_vec=_obs())
    assert bool(learner.model.replay_buffer.dones[0]), "closed-trade transitions must be terminal"


def test_snapshot_writes_weights_and_buffer(learner, tmp_path):
    """Weights alone would come back having forgotten every experience since the last snapshot."""
    import os

    learner.record(order_id=1, obs_vec=_obs(), action=_action())
    learner.complete(order_id=1, reward=0.1, final_obs_vec=_obs())
    learner.snapshot()

    assert os.path.exists(learner.model_path), "weights must be written"
    assert os.path.exists(learner.buffer_path), "replay buffer must be written alongside them"


def test_pending_decisions_are_bounded(learner):
    """A caller that opens decisions and never closes them must not grow memory without limit."""
    for i in range(MAX_PENDING + 50):
        learner.record(order_id=i + 1, obs_vec=_obs(), action=_action())
    assert learner.stats()["pending"] <= MAX_PENDING


def test_training_failure_does_not_break_serving(learner, monkeypatch):
    """Answering /predict is the service's first duty — a frozen-but-serving model beats a dead one."""
    def boom(*a, **kw):
        raise RuntimeError("gradient exploded")

    monkeypatch.setattr(learner.model, "train", boom)
    for i in range(5):
        learner.record(order_id=i + 1, obs_vec=_obs(), action=_action())
        # Must not raise despite every gradient step failing.
        learner.complete(order_id=i + 1, reward=0.1, final_obs_vec=_obs())

    assert learner.stats()["buffer_size"] == 5, "experience is still collected"
    assert learner.updates == 0


def test_reward_is_normalized_by_account_size():
    """A $5 win means something different on a $100 account than on a $10,000 one; a policy trained
    on raw dollars would not transfer when the balance is reconfigured (CLAUDE.md §15.11)."""
    assert reward_from_outcome(5.0, 100.0) == pytest.approx(0.05)
    assert reward_from_outcome(5.0, 10_000.0) == pytest.approx(0.0005)
    assert reward_from_outcome(-10.0, 100.0) == pytest.approx(-0.1)
    # A zero/unknown account size must not divide by zero.
    assert reward_from_outcome(5.0, 0.0) == 0.0
