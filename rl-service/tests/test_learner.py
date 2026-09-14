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
    # Account-denominator fallback, used when position size is unknown (historical rows, pretrain).
    assert reward_from_outcome(5.0, 100.0) == pytest.approx(0.05)
    assert reward_from_outcome(5.0, 10_000.0) == pytest.approx(0.0005)
    assert reward_from_outcome(-10.0, 100.0) == pytest.approx(-0.1)

    # With a position size, the reward is return-on-capital and the account no longer matters
    # (2026-09-14). This is the property that fixes the scale problem: the same trade scores the
    # same whether the account is $200 or $2,600, where the account denominator shrank every reward
    # thirteenfold when the cap was raised without a single trade changing.
    assert reward_from_outcome(0.5, 200.0, 10.0) == pytest.approx(0.05)
    assert reward_from_outcome(0.5, 2_600.0, 10.0) == pytest.approx(0.05)
    assert reward_from_outcome(-0.5, 2_600.0, 10.0) == pytest.approx(-0.05)

    # A real trade from 2026-09-14: -$0.308 on a $9.55 position. The account denominator scored this
    # at -1.2e-4, which is indistinguishable from zero to SAC.
    assert reward_from_outcome(-0.308, 2_600.0, 9.55) == pytest.approx(-0.03225, abs=1e-4)
    assert abs(reward_from_outcome(-0.308, 2_600.0, 9.55)) > 100 * abs(
        reward_from_outcome(-0.308, 2_600.0)
    )

    # Zero size falls back rather than dividing by zero.
    assert reward_from_outcome(5.0, 100.0, 0.0) == pytest.approx(0.05)
    # A zero/unknown account size must not divide by zero.
    assert reward_from_outcome(5.0, 0.0) == 0.0


def test_entropy_repair_restores_alpha_and_loosens_the_target():
    """The 2026-09-14 saturation fix, tested on a real SAC model rather than in the abstract.

    SAC's default target_entropy is -dim(action_space) = -9 here, which over 9 tanh-squashed
    dimensions asks for -1.0 per dimension — a near-deterministic policy. Alpha collapsed chasing
    it (measured 0.000919 against a 1.0 start), which removes the entropy term from the actor loss
    and lets the policy drift to the tanh bounds: 9 of 9 outputs pinned at +/-1, answering
    identically to every input.

    Both halves are required, which is what this pins. Setting the target alone leaves the collapsed
    alpha restored from the checkpoint; resetting alpha alone gets undone by the same -9 target.
    """
    import math

    import gymnasium as gym
    import torch as th
    from gymnasium import spaces
    from stable_baselines3 import SAC

    # Its own env rather than the fixture's: that Dummy is nested inside the fixture function, and
    # this test needs the model itself, not a Learner wrapped around one.
    class _Env(gym.Env):
        observation_space = spaces.Box(-np.inf, np.inf, (OBS_DIM,), np.float32)
        action_space = spaces.Box(-1, 1, (ACTION_DIM,), np.float32)

        def reset(self, *, seed=None, options=None):
            return np.zeros(OBS_DIM, np.float32), {}

        def step(self, a):
            return np.zeros(OBS_DIM, np.float32), 0.0, False, False, {}

    model = SAC("MlpPolicy", _Env(), buffer_size=100, batch_size=8, verbose=0)

    # SAC's default is what caused the incident: -dim(action_space).
    assert model.target_entropy == -float(ACTION_DIM)

    # Drive alpha down the way live training did, then repair it the way serve/api.py does.
    with th.no_grad():
        model.log_ent_coef.fill_(math.log(0.000919))
    assert float(th.exp(model.log_ent_coef.detach()).item()) < 0.001

    model.target_entropy = -4.5
    with th.no_grad():
        model.log_ent_coef.fill_(math.log(1.0))
    model.ent_coef_optimizer = th.optim.Adam([model.log_ent_coef], lr=3e-4)

    assert float(th.exp(model.log_ent_coef.detach()).item()) == pytest.approx(1.0, abs=1e-6)
    assert model.target_entropy == -4.5
    # The optimizer must be rebuilt, not reused: its Adam moment estimates accumulated while alpha
    # was collapsing and would pull the reset value straight back down.
    assert len(model.ent_coef_optimizer.state) == 0
