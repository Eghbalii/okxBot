package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// RLHealth mirrors rl_service/serve/api.py's GET /health response. model_loaded=false with the
// service otherwise reachable means it's up but serving flat/no-op actions (CLAUDE.md §2's
// fail-safe path) — a materially different state from "down" that the panel must show distinctly.
type RLHealth struct {
	Reachable   bool   `json:"reachable"`
	Status      string `json:"status,omitempty"`
	ModelLoaded bool   `json:"modelLoaded"`
	Error       string `json:"error,omitempty"`
}

// FetchRLHealth calls the RL inference service's /health endpoint.
func FetchRLHealth(ctx context.Context, baseURL string) RLHealth {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return RLHealth{Error: err.Error()}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return RLHealth{Error: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return RLHealth{Error: fmt.Sprintf("rl-service /health returned status %d", resp.StatusCode)}
	}

	var body struct {
		Status      string `json:"status"`
		ModelLoaded bool   `json:"model_loaded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return RLHealth{Error: fmt.Sprintf("decode /health response: %v", err)}
	}
	return RLHealth{Reachable: true, Status: body.Status, ModelLoaded: body.ModelLoaded}
}
