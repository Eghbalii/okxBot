-- Separate "a person turned this token off" from "the affordability service turned it off"
-- (2026-09-09: operator reported that unchecking a token in the panel appeared to do nothing —
-- Save persisted correctly, and then the trader's own restart put the token straight back).
--
-- The cause: disabled_inst_ids was ONE list with no record of who wrote each entry, and
-- AffordabilityService.applyPlan re-enables any disabled token that has become affordable again.
-- A manually-disabled token is affordable by definition in the normal case, so it was re-enabled
-- within seconds of every save — the operator's decision was not lost, it was overruled.
--
-- auto_disabled_inst_ids records the service's OWN entries. It re-enables only tokens in this
-- list, so an operator's choice now outlives a restart and stays until a person reverses it.
--
-- Backfill is deliberately empty rather than a copy of disabled_inst_ids: every existing entry has
-- unknown provenance, and treating unknown entries as manual is the safe direction — it can only
-- leave a token off until someone turns it back on, whereas guessing "auto" would re-enable
-- tokens a person had deliberately disabled, which is the exact bug being fixed.
ALTER TABLE paper_trading_config
    ADD COLUMN IF NOT EXISTS auto_disabled_inst_ids TEXT[] NOT NULL DEFAULT '{}';
