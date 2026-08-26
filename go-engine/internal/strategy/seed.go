package strategy

import (
	"context"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// SeedOrigins ensures every built-in Factories entry has a corresponding locked origin row
// (CLAUDE.md §11.3) in repo — idempotent, so it's safe to call on every startup of any service
// that touches strategies (cmd/paper-trader, cmd/api) rather than depending on run order between
// them: cmd/api's Strategies panel must work even if paper-trader has never run.
func SeedOrigins(ctx context.Context, repo port.Repository) error {
	existing, err := repo.ListStrategies(ctx, "", false)
	if err != nil {
		return err
	}
	haveKind := make(map[string]bool, len(existing))
	for _, s := range existing {
		if s.IsOrigin {
			haveKind[s.Kind] = true
		}
	}
	for kind := range Factories {
		if haveKind[kind] {
			continue
		}
		if _, err := repo.CreateStrategy(ctx, port.StrategyConfig{
			Name:     kind,
			Kind:     kind,
			Config:   []byte("{}"),
			Enabled:  true,
			IsOrigin: true,
		}); err != nil {
			return err
		}
	}
	return nil
}
