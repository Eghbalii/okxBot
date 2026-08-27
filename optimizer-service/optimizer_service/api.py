"""Optuna candidate-proposal sidecar for cmd/strategy-optimizer (CLAUDE.md §16.2).

Deliberately a brand-new, minimal service — not folded into rl-service, since it has no
ML-model-loading/torch concerns (§15.1's Dockerfile note: rl-service's CPU-only-torch workaround
does not apply here at all). This service is stateless from Go's perspective per call: Go
(cmd/strategy-optimizer) owns trial lifecycle/bookkeeping (running strategy.Strategy against real
market data, judging SL/TP-touch win/loss) and only calls here to ask "what parameters should I
try next" and to report back "here's how that one did." Optuna's own study state lives only in
this process's memory (`optuna.create_study` per study id, kept in `_studies` below) — a sidecar
restart just starts fresh studies, which is fine per §16.3's "disposable, not durable" framing for
trial state; only a *winning* candidate ever becomes durable, and that persistence happens on the
Go side (port.Repository.CreateStrategy/CreateAssignment), not here.

Uses Optuna's ask/tell API (not the higher-level `optimize()` loop) because Go drives the
trial loop — a `study.ask()` reserves a `trial.number` we return as the candidate's `trial_id`, so
a later /report can look the exact same optuna.Trial back up via `study.trials[trial_id]` and call
`study.tell(trial, score)` on it.
"""
from __future__ import annotations

import logging
from typing import Dict, List

import optuna
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

logging.getLogger("optuna").setLevel(logging.WARNING)
logger = logging.getLogger("optimizer_service")

app = FastAPI(title="okxBot strategy-optimizer sidecar")

# study_id -> optuna.Study. study_id is "{inst_id}:{kind}" by convention (CLAUDE.md §16.2), but
# this service treats it as an opaque string — Go owns the naming convention.
_studies: Dict[str, optuna.Study] = {}


def _get_or_create_study(study_id: str) -> optuna.Study:
    study = _studies.get(study_id)
    if study is None:
        # TPE (Tree-structured Parzen Estimator) is Optuna's default sampler and a solid general
        # choice for this problem shape (a handful of continuous/int parameters, noisy win/loss
        # feedback) — no need to hand-pick a sampler for v1.
        study = optuna.create_study(direction="maximize", sampler=optuna.samplers.TPESampler())
        _studies[study_id] = study
    return study


class ParamSpec(BaseModel):
    name: str
    min: float
    max: float


class SuggestRequest(BaseModel):
    study_id: str
    param_specs: List[ParamSpec]
    n_candidates: int = Field(default=1, ge=1, le=100)


class Candidate(BaseModel):
    trial_id: int
    params: Dict[str, float]


class SuggestResponse(BaseModel):
    study_id: str
    candidates: List[Candidate]


class ReportRequest(BaseModel):
    study_id: str
    trial_id: int
    score: float


class ReportResponse(BaseModel):
    ok: bool


@app.get("/health")
def health():
    return {"status": "ok"}


@app.post("/suggest", response_model=SuggestResponse)
def suggest(req: SuggestRequest) -> SuggestResponse:
    if not req.param_specs:
        raise HTTPException(status_code=400, detail="param_specs must be non-empty")
    for spec in req.param_specs:
        if spec.min > spec.max:
            raise HTTPException(
                status_code=400,
                detail=f"param {spec.name!r}: min ({spec.min}) > max ({spec.max})",
            )

    study = _get_or_create_study(req.study_id)
    candidates: List[Candidate] = []
    for _ in range(req.n_candidates):
        trial = study.ask()
        params = {spec.name: trial.suggest_float(spec.name, spec.min, spec.max) for spec in req.param_specs}
        candidates.append(Candidate(trial_id=trial.number, params=params))

    return SuggestResponse(study_id=req.study_id, candidates=candidates)


@app.post("/report", response_model=ReportResponse)
def report(req: ReportRequest) -> ReportResponse:
    study = _studies.get(req.study_id)
    if study is None:
        raise HTTPException(status_code=404, detail=f"unknown study_id {req.study_id!r}")
    if req.trial_id < 0 or req.trial_id >= len(study.trials):
        raise HTTPException(status_code=404, detail=f"unknown trial_id {req.trial_id} for study {req.study_id!r}")

    # study.tell() accepts a trial *number* (int) directly — passing the FrozenTrial object from
    # study.trials[...] instead raises "Trial must be a trial object or trial number" on current
    # Optuna (tell() only accepts a live Trial or an int, not a FrozenTrial).
    study.tell(req.trial_id, req.score)
    return ReportResponse(ok=True)
