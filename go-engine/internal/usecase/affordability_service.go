package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// DefaultAffordabilityInterval is how often the roster is re-evaluated when no interval is set.
// 15 minutes is deliberately slow: the inputs are the account balance and instrument prices, and a
// token crossing the affordability line is a slow, structural change, not a tick-by-tick one.
// Checking faster would mostly re-derive the same answer while adding exchange calls to a shared
// rate-limit budget the live trading path also draws on.
const DefaultAffordabilityInterval = 15 * time.Minute

// affordabilityFixedPointLimit bounds the iteration below. Each pass can only ever DISABLE tokens
// (which raises the remaining share) or ENABLE them (which lowers it), so the loop converges
// quickly; the bound exists so a pathological input cannot spin forever, not because convergence
// is expected to be slow.
const affordabilityFixedPointLimit = 10

// AffordabilityService keeps the active token roster in step with what the account can actually
// afford to trade (2026-09-08 request).
//
// Why this needs to be automatic rather than a one-time config: the per-token budget is
// equity/tokenCount, so it moves with the account. Profit raises it and can bring a previously
// unaffordable token back; losses lower it and can push a token out. Left to a human, the roster
// silently drifts out of step with the balance and the engine spends strategy slots on tokens whose
// every signal can only be declined at sizing time.
//
// It writes to the SAME disabled_inst_ids the panel's own token control uses, so an operator can
// still see and edit the result — this service is not a parallel mechanism, it drives the existing
// one. Note that means a token an operator disabled by hand can be re-enabled by this service once
// it becomes affordable; that is the intended behavior for an affordability-driven roster, and an
// operator who wants a token permanently out should remove it from trading.inst_ids instead.
type AffordabilityService struct {
	Repo     port.Repository
	Exchange port.ExchangeClient
	Logger   *slog.Logger

	// Mode is the trading mode whose config is managed ("real" or "paper").
	Mode string
	// AllTokens is the full configured roster (trading.inst_ids), in short-symbol form.
	AllTokens []string
	// SymbolMap resolves a short symbol to the OKX instId used for instrument/ticker lookups.
	SymbolMap okx.SymbolMap
	// ExecInstType is the instType those lookups use (e.g. "FUTURES" for X-Perp).
	ExecInstType string

	// MaxPositionPct is account.max_position_pct, applied as a ceiling on the even share exactly
	// as sizing applies it, so this service judges affordability against the budget sizing will
	// actually use rather than a looser one.
	MaxPositionPct decimal.Decimal
	// MaxLeverage is the ceiling the model's own leverage choice is mapped into (risk.max_leverage).
	// Affordability is judged at this maximum deliberately: the question is whether a token is
	// EVER tradeable at this account size, and a token the model could open at 10x should not be
	// taken offline because it would be unaffordable at the 1x it might also choose. Sizing then
	// declines the individual order if the model happens to pick a leverage too low to reach one
	// contract — a per-order decision, not a reason to disable the token for everyone.
	MaxLeverage decimal.Decimal
	// Interval defaults to DefaultAffordabilityInterval.
	Interval time.Duration

	// DryRun logs what would change without writing it — for verifying the decision on a live
	// account before letting it act.
	DryRun bool

	// OnRosterChange is called with the full disabled set after every applied change, so running
	// engines can update their own open gate immediately. Without it a roster change only takes
	// effect at the next process restart, since each engine captures its token's flag once at
	// construction — observed live: PUMP and PEPE were auto-disabled 1.5 seconds after trader
	// started and went on attempting opens for 20 minutes.
	OnRosterChange func(disabled []string)
}

func (s *AffordabilityService) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *AffordabilityService) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return DefaultAffordabilityInterval
}

// leverage is the figure affordability is judged at — the configured maximum, defaulting to 1x
// when unset so an unconfigured service errs toward declaring MORE tokens unaffordable rather than
// fewer.
func (s *AffordabilityService) leverage() decimal.Decimal {
	if s.MaxLeverage.IsPositive() {
		return s.MaxLeverage
	}
	return decimal.NewFromInt(1)
}

func (s *AffordabilityService) mode() string {
	if s.Mode == "" {
		return "real"
	}
	return s.Mode
}

// Run evaluates the roster immediately, then on every interval tick until ctx is cancelled.
// Evaluating on start matters: a restart is exactly when the balance may have moved while this was
// not running.
func (s *AffordabilityService) Run(ctx context.Context) error {
	if err := s.RunOnce(ctx); err != nil {
		s.logger().Warn("affordability: first evaluation failed", "error", err)
	}
	t := time.NewTicker(s.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.RunOnce(ctx); err != nil {
				s.logger().Warn("affordability: evaluation failed", "error", err)
			}
		}
	}
}

// RunOnce performs a single evaluation and applies any roster change.
func (s *AffordabilityService) RunOnce(ctx context.Context) error {
	logger := s.logger()

	equity, err := s.tradableEquity(ctx)
	if err != nil {
		return err
	}
	if !equity.IsPositive() {
		// A drained or unreadable account is not evidence that every token should be disabled —
		// that would take the whole roster offline on a transient read failure and require a human
		// to notice. Leave the roster alone and say so.
		logger.Warn("affordability: no positive tradable equity, leaving the roster unchanged",
			"mode", s.mode(), "equity", equity)
		return nil
	}

	cfg, err := s.Repo.GetPaperTradingConfig(ctx, s.mode())
	if err != nil {
		return fmt.Errorf("read %s trading config: %w", s.mode(), err)
	}
	disabled := make(map[string]bool, len(cfg.DisabledInstIDs))
	for _, t := range cfg.DisabledInstIDs {
		disabled[t] = true
	}

	instruments, prices := s.marketData(logger)
	if len(instruments) == 0 {
		// Nothing could be read at all — almost certainly the exchange or gateway, not a real
		// affordability change. Acting on this would disable the entire roster.
		logger.Warn("affordability: no instrument data available, leaving the roster unchanged")
		return nil
	}

	plan := s.plan(instruments, prices, disabled, equity)
	if !plan.Changed() {
		logger.Debug("affordability: roster already correct", "budget", plan.BudgetUSD.StringFixed(4),
			"unaffordable", plan.Unaffordable)
		return nil
	}

	logger.Info("affordability: roster change",
		"mode", s.mode(), "equity", equity.StringFixed(4), "budget", plan.BudgetUSD.StringFixed(4),
		"disable", plan.ToDisable, "enable", plan.ToEnable, "dryRun", s.DryRun)
	if s.DryRun {
		return nil
	}

	next := s.applyPlan(disabled, plan)
	if _, err := s.Repo.SavePaperTradingConfig(ctx, s.mode(), port.PaperTradingConfigPatch{DisabledInstIDs: &next}); err != nil {
		return fmt.Errorf("save %s trading config: %w", s.mode(), err)
	}
	// Notify AFTER the write succeeds: an engine must never start refusing opens on the strength of
	// a decision that was not durably recorded, since a restart would then silently undo it.
	if s.OnRosterChange != nil {
		s.OnRosterChange(next)
	}
	return nil
}

// plan decides the roster in one monotone pass, greedily and deterministically.
//
// The obvious approach — iterate until the set stops changing — does NOT converge, and this was
// caught by simulating it against the live roster before deploying. PEPE (minimum $3.64) oscillates
// forever on a $20 account: at 5 active tokens the budget is $4.00 so it is affordable and gets
// enabled, which makes 6 active and a $3.33 budget, so it is unaffordable and gets disabled, which
// makes 5 again. There is no fixed point to find, because enabling a token is what makes it
// unaffordable.
//
// So instead: sort tokens by minimum notional ascending and admit them greedily, each time checking
// against the budget implied by the roster INCLUDING that token. A token is admitted only if it
// remains affordable once its own admission has diluted the budget — which is exactly the
// self-consistency the iterative version kept violating. Cheapest-first is what makes the result
// both deterministic and maximal: admitting an expensive token early would crowd out several cheap
// ones that together fit.
//
// Tokens with unreadable instrument/price data are neither admitted nor disabled; they keep their
// current state, since acting on missing data would take a tradeable token offline on a transient
// API failure.
func (s *AffordabilityService) plan(
	instruments map[string]domain.Instrument,
	prices map[string]decimal.Decimal,
	startDisabled map[string]bool,
	equity decimal.Decimal,
) AffordabilityPlan {
	type candidate struct {
		token       string
		minNotional decimal.Decimal
	}
	var known []candidate
	unknown := map[string]bool{}
	for _, tok := range s.AllTokens {
		inst, hasInst := instruments[tok]
		price, hasPrice := prices[tok]
		if !hasInst || !hasPrice || !price.IsPositive() {
			unknown[tok] = true
			continue
		}
		known = append(known, candidate{tok, MinNotionalFor(inst, price)})
	}
	sort.Slice(known, func(i, j int) bool { return known[i].minNotional.LessThan(known[j].minNotional) })

	// An unknown token still occupies a roster slot if it is currently active, so it must count
	// toward the divisor — otherwise admitting others against a budget that ignores it would
	// over-commit the account.
	unknownActive := 0
	for tok := range unknown {
		if !startDisabled[tok] {
			unknownActive++
		}
	}

	admitted := map[string]bool{}
	var settledBudget decimal.Decimal
	for i, c := range known {
		// The budget if this token and every cheaper one were active.
		count := i + 1 + unknownActive
		budget := PerTokenBudget(equity, count, s.MaxPositionPct)
		// Buying power, not the raw margin budget: a $2 margin share at 10x opens a $20 position,
		// which is what an instrument's minimum notional has to be compared against.
		if BuyingPower(budget, s.leverage()).LessThan(c.minNotional) {
			// Too expensive at its own implied budget — and since the list is sorted ascending and
			// each later token is dearer while the budget only shrinks, nothing after it can fit
			// either.
			break
		}
		admitted[c.token] = true
		settledBudget = budget
	}
	if settledBudget.IsZero() {
		settledBudget = PerTokenBudget(equity, len(s.AllTokens), s.MaxPositionPct)
	}

	final := make(map[string]bool, len(s.AllTokens))
	var unaffordable []string
	for _, tok := range s.AllTokens {
		if unknown[tok] {
			final[tok] = startDisabled[tok] // untouched
			continue
		}
		if !admitted[tok] {
			final[tok] = true
			unaffordable = append(unaffordable, tok)
		}
	}
	return s.diffFrom(startDisabled, final, settledBudget, unaffordable)
}

// diffFrom expresses a converged disabled-set as changes against the original one.
func (s *AffordabilityService) diffFrom(original, final map[string]bool, budget decimal.Decimal, unaffordable []string) AffordabilityPlan {
	plan := AffordabilityPlan{BudgetUSD: budget, Unaffordable: unaffordable}
	for _, t := range s.AllTokens {
		switch {
		case final[t] && !original[t]:
			plan.ToDisable = append(plan.ToDisable, t)
		case !final[t] && original[t]:
			plan.ToEnable = append(plan.ToEnable, t)
		}
	}
	return plan
}

// applyPlan produces the new disabled list, sorted so a config write is stable and a diff between
// two saves reflects a real change rather than map iteration order.
func (s *AffordabilityService) applyPlan(disabled map[string]bool, plan AffordabilityPlan) []string {
	next := make(map[string]bool, len(disabled))
	for k, v := range disabled {
		next[k] = v
	}
	for _, t := range plan.ToDisable {
		next[t] = true
	}
	for _, t := range plan.ToEnable {
		delete(next, t)
	}
	out := make([]string, 0, len(next))
	for t, on := range next {
		if on {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// tradableEquity is what sizing may draw against: the operator's trading cap when one is set,
// otherwise the account's recorded equity. Read from the same repository row sizing reads, so this
// service can never judge affordability against a different number than the one that will be used.
func (s *AffordabilityService) tradableEquity(ctx context.Context) (decimal.Decimal, error) {
	acct, err := s.Repo.GetAccountEquity(ctx, s.mode(), decimal.Zero)
	if err != nil {
		return decimal.Zero, fmt.Errorf("read %s account equity: %w", s.mode(), err)
	}
	return acct.EquityUSD, nil
}

// marketData fetches instrument metadata and a current price per token. A token whose lookup fails
// is simply absent from the returned maps, and PlanAffordability leaves such a token alone — a
// transient API error must never disable a tradeable token.
func (s *AffordabilityService) marketData(logger *slog.Logger) (map[string]domain.Instrument, map[string]decimal.Decimal) {
	instruments := make(map[string]domain.Instrument, len(s.AllTokens))
	prices := make(map[string]decimal.Decimal, len(s.AllTokens))
	for _, tok := range s.AllTokens {
		instID, err := s.SymbolMap.Resolve(tok)
		if err != nil {
			logger.Warn("affordability: unmapped symbol, skipping", "symbol", tok, "error", err)
			continue
		}
		// Argument order is (instType, instID) — not the other way round. Getting this backwards
		// produced a request for instId=FUTURES against every token, which failed uniformly and
		// (correctly, per the fail-safe below) left the whole roster untouched.
		inst, err := s.Exchange.GetInstrument(s.ExecInstType, instID)
		if err != nil {
			logger.Warn("affordability: instrument lookup failed, leaving token unchanged",
				"symbol", tok, "instId", instID, "error", err)
			continue
		}
		ticker, err := s.Exchange.GetTicker(instID)
		if err != nil {
			logger.Warn("affordability: ticker lookup failed, leaving token unchanged",
				"symbol", tok, "instId", instID, "error", err)
			continue
		}
		instruments[tok] = inst
		prices[tok] = ticker.Last
	}
	return instruments, prices
}

// AffordabilityReport is a read-only snapshot of the current affordability picture, for the panel's
// Manage Tokens modal (2026-09-08 request). Deliberately separate from RunOnce: this must never
// change the roster, only describe it, so opening the modal cannot enable or disable anything.
type AffordabilityReport struct {
	// BudgetUSD is the per-token budget the tokens below were judged against.
	BudgetUSD decimal.Decimal
	Tokens    []TokenAffordability
}

// TokenAffordability is one token's row in a report.
type TokenAffordability struct {
	InstID string
	// MinNotionalUSD is the exchange's smallest acceptable position for this instrument. Zero and
	// meaningless when Unknown.
	MinNotionalUSD decimal.Decimal
	Affordable     bool
	Disabled       bool
	// AutoDisabled means the affordability rule is what excludes this token — it is disabled and
	// the settled roster does not admit it. Distinct from !Affordable, which compares against the
	// current budget: a token can look affordable at a budget that only exists BECAUSE it is
	// excluded (PEPE at $3.65 against a $4.00 budget that becomes $3.33 the moment it is admitted).
	AutoDisabled bool
	// Unknown means the instrument or price could not be read, so no judgement was made — such a
	// token is never auto-disabled, since a transient API failure must not take a tradeable token
	// offline.
	Unknown bool
}

// Report describes the current picture without changing anything. The budget is computed against
// the roster as it stands right now (not the settled one RunOnce would converge to), because that
// is the budget sizing would actually use for the next order.
func (s *AffordabilityService) Report(ctx context.Context) (AffordabilityReport, error) {
	equity, err := s.tradableEquity(ctx)
	if err != nil {
		return AffordabilityReport{}, err
	}

	cfg, err := s.Repo.GetPaperTradingConfig(ctx, s.mode())
	if err != nil {
		return AffordabilityReport{}, fmt.Errorf("read %s trading config: %w", s.mode(), err)
	}
	disabled := make(map[string]bool, len(cfg.DisabledInstIDs))
	for _, t := range cfg.DisabledInstIDs {
		disabled[t] = true
	}

	activeCount := 0
	for _, t := range s.AllTokens {
		if !disabled[t] {
			activeCount++
		}
	}
	budget := PerTokenBudget(equity, activeCount, s.MaxPositionPct)

	instruments, prices := s.marketData(s.logger())

	// The set the admission rule would settle on, so AutoDisabled reflects the real decision rather
	// than a per-token budget comparison that cannot account for a token's own dilution.
	settled := s.plan(instruments, prices, disabled, equity)
	admitted := make(map[string]bool, len(s.AllTokens))
	for _, tok := range s.AllTokens {
		admitted[tok] = !disabled[tok]
	}
	for _, tok := range settled.ToDisable {
		admitted[tok] = false
	}
	for _, tok := range settled.ToEnable {
		admitted[tok] = true
	}

	report := AffordabilityReport{BudgetUSD: budget, Tokens: make([]TokenAffordability, 0, len(s.AllTokens))}
	for _, tok := range s.AllTokens {
		row := TokenAffordability{InstID: tok, Disabled: disabled[tok]}
		inst, hasInst := instruments[tok]
		price, hasPrice := prices[tok]
		if !hasInst || !hasPrice || !price.IsPositive() {
			row.Unknown = true
			report.Tokens = append(report.Tokens, row)
			continue
		}
		aff := CanAfford(inst, price, budget, s.leverage())
		row.MinNotionalUSD = aff.MinNotionalUSD
		row.Affordable = aff.Affordable
		// Ask the actual admission rule rather than inferring from Affordable, so the panel's tag
		// matches what the service would really do on its next pass.
		row.AutoDisabled = row.Disabled && !admitted[tok]
		report.Tokens = append(report.Tokens, row)
	}
	return report, nil
}
