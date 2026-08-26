# Non-strategy Pine scripts

These files were dropped alongside the real strategies in `../` but are not signal-generating
strategies themselves — set aside here rather than converted:

- `strategy_Trading Report Generator from CSV.pine` — replays a pasted CSV of past trades, no
  entry/exit logic of its own.
- `strategy_Built-in Kelly ratio for dynamic position sizing.pine` — Kelly-ratio position-sizing
  math; its entry condition is a placeholder crossover, not a real signal.
- `strategy_Сalculation a position size based on risk.pine` — risk-% based position-sizing math;
  entries are literally `bar_index % 333 == 0` (random), by the author's own comment.
- `strategy_Oscillator Evaluator (Analysis tool).pine` — lets you swap between 6 generic
  strategy *shapes* applied to a placeholder oscillator input; an analysis tool, not a strategy.
- `strategy_Ultimate Strategy Template.pine` — risk-management/execution scaffold (stop types,
  streak limits, session filters); expects an external entry signal to be plugged in.
- `strategy_Template Trailing Strategy (Backtester).pine` — exit/execution scaffold, explicitly
  described by its own author as meant to pair with a separate signal indicator. Also
  CC BY-NC-SA (noncommercial) licensed, unlike most of the others here.

Revisit if/when position-sizing helpers (Kelly, risk-%) or a generic execution/trailing-stop
scaffold become worth building as shared Go utilities — the math in the first two is reusable
even though the files aren't standalone strategies.
