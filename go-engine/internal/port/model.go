package port

import (
	"context"

	"github.com/rez/okxBot/go-engine/internal/domain"
)

// ModelClient is the port use-cases depend on to get the RL agent's trading decision. Implemented
// by internal/rlclient (calls the Python FastAPI inference service). See CLAUDE.md §10.
type ModelClient interface {
	Predict(ctx context.Context, obs domain.Observation) (*domain.Action, error)
}
