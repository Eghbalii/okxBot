package domain

import "fmt"

// ValidationError names exactly which part of an observation is unusable, so a skipped model call
// can be counted by reason rather than as an undifferentiated failure.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("observation: %s: %s", e.Field, e.Reason)
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// ValidationField returns the field name from a ValidationError, or "" for any other error. Used as
// a metric label so "why was the model not consulted" is answerable from Prometheus rather than by
// grepping logs.
func ValidationField(err error) string {
	if ve, ok := err.(*ValidationError); ok {
		return ve.Field
	}
	return ""
}

// Validate reports whether this observation can be vectorized at all.
//
// WHY THIS EXISTS, and why it returns an error rather than repairing anything. Before v8,
// rl_service padded or truncated whatever it was given to whatever width the loaded model wanted —
// so an observation missing ten values still produced a confident-looking action, and did for
// weeks with no log, no metric and no failure to notice (docs/RL_V8_PLAN.md). The fix is not a
// better repair; it is refusing to guess.
//
// The caller's correct response to an error here is to SKIP the model call entirely, not to send it
// and let rl_service reject it. A rejected call leaves a pending decision in the learner that never
// receives its reward — reopening the exact gap §15.12 closed, where the model was asked questions
// and never told how any answer turned out.
//
// A short candle window right after startup is the normal case this fires on, and that is correct:
// no decision should be made on data that does not exist yet. The operator confirmed the resulting
// quiet period is the intended behaviour, not a regression.
func (o Observation) Validate() error {
	if o.SchemaVersion != ObservationSchemaVersion {
		return invalid("schema_version", "got %d, want %d", o.SchemaVersion, ObservationSchemaVersion)
	}
	if o.InstID == "" {
		return invalid("inst_id", "empty")
	}
	if !o.LastPrice.IsPositive() {
		return invalid("last_price", "must be positive, got %s", o.LastPrice)
	}
	// A zero equity is not merely one wrong number: account_block derives equity_ratio,
	// exposure_ratio and size/equity from it, so all three collapse to zero and the model is told
	// the account is empty. In v7 a failed GetAccountEquity produced exactly this on a Warn log.
	if !o.AccountEquityUSD.IsPositive() {
		return invalid("account_equity_usd", "must be positive, got %s", o.AccountEquityUSD)
	}
	if !o.MaxPositionPct.IsPositive() {
		return invalid("max_position_pct", "must be positive, got %s", o.MaxPositionPct)
	}
	if !o.MaxLeverage.IsPositive() {
		return invalid("max_leverage", "must be positive, got %s", o.MaxLeverage)
	}

	if len(o.Timeframes) != 1 {
		return invalid("timeframes", "want exactly 1 block, got %d", len(o.Timeframes))
	}
	if err := o.Timeframes[0].validate(); err != nil {
		return err
	}
	if err := o.BTC.validate(); err != nil {
		return err
	}

	if o.Category == "" {
		return invalid("category", "empty")
	}
	// A terminal call is the reward delivery path (§15.10). Arriving without the position it is
	// reporting on would train the policy on an outcome detached from any decision.
	if IsTerminalCategory(o.Category) && o.OrderID == 0 {
		return invalid("order_id", "terminal category %q needs the order it is reporting on", o.Category)
	}
	return nil
}

func (m MarketBlock) validate() error {
	if m.Bar == "" {
		return invalid("timeframe.bar", "empty")
	}
	if len(m.Indicators) != IndicatorsPerTimeframe {
		return invalid("timeframe.indicators", "got %d, want exactly %d",
			len(m.Indicators), IndicatorsPerTimeframe)
	}
	if len(m.ClosePctChanges) != ReturnsWindow {
		return invalid("timeframe.returns", "got %d, want exactly %d — a short candle window is a "+
			"reason to skip the model call, not to pad it", len(m.ClosePctChanges), ReturnsWindow)
	}
	if !m.Close.IsPositive() {
		return invalid("timeframe.close", "must be positive, got %s", m.Close)
	}
	return nil
}

func (b BTCContext) validate() error {
	// BTC is not optional. Every other input is intra-token, so a missing BTC block leaves the
	// model blind to the market-wide move that drives most altcoin reversals — which is the whole
	// reason this block was added.
	if len(b.ClosePctChanges) != ReturnsWindow {
		return invalid("btc.returns", "got %d, want exactly %d",
			len(b.ClosePctChanges), ReturnsWindow)
	}
	if !b.Close.IsPositive() {
		return invalid("btc.close", "must be positive, got %s", b.Close)
	}
	return nil
}
