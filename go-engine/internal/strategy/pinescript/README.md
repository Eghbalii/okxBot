# Pine Script source drop folder

This directory holds original TradingView Pine Script (`.pine`) source files that are the
*inputs* to hand/LLM-converted Go strategy implementations. Nothing here is compiled or executed
directly — it's the reference source kept alongside its Go port for traceability (and, since
these scripts are pulled from public repos/authors, for attribution when the project goes open
source).

## Convention

- One file per strategy: `<strategy_id>.pine`, where `<strategy_id>` matches the `Name()` string
  the converted Go strategy will return (e.g. `macd_cross.pine` -> `Name() == "macd_cross"`).
- Add a one-line header comment in the `.pine` file noting its source (repo URL / author) for
  attribution.
- Conversion is currently a manual step: drop the file here, then have it converted to a Go
  `strategy.Strategy` implementation at `../<strategy_id>.go` (same `strategy` package as
  `rsi_sma.go` — a `generated/` subdirectory can't share the package, so converted strategies live
  flat alongside hand-written ones), and register it in the strategy registry wiring.
- Files that turn out not to be real signal-generating strategies (position-sizing demos,
  analysis/template tools, report generators) are moved to `_utility/` instead of converted — see
  `_utility/README.md`.
- Planned: the panel (`panel/`, CLAUDE.md §11, not yet built) will let a user paste Pine Script
  directly and save it, calling the same conversion step programmatically instead of via a
  dropped file — so the conversion logic itself should stay decoupled from "reads from disk"
  once it exists as more than a manual process.
