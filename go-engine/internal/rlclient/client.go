// Package rlclient calls the Python RL inference service to get the agent's trading action.
package rlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client calls the FastAPI inference server's /predict endpoint.
type Client struct {
	BaseURL    string
	httpClient *http.Client
}

// New creates an rlclient.Client pointed at the given base URL (e.g. http://localhost:8000).
func New(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Observation is the feature vector sent to the RL service for one inference step.
// Field names match rl_service/env observation construction — keep both sides in sync.
type Observation struct {
	InstID           string    `json:"inst_id"`
	MidPrice         float64   `json:"mid_price"`
	Position         float64   `json:"position"` // signed current position size
	CurrentLeverage  float64   `json:"current_leverage"`
	UnrealizedPnLPct float64   `json:"unrealized_pnl_pct"`
	EquityUSD        float64   `json:"equity_usd"`
	Features         []float64 `json:"features"` // rolling window of engineered features
}

// Action is the RL agent's decision returned by the inference service.
type Action struct {
	TargetExposure float64 `json:"target_exposure"` // in [-1, 1]
	LeverageFrac   float64 `json:"leverage_frac"`   // in [0, 1], mapped to [1x, max_leverage]
	Confidence     float64 `json:"confidence"`
}

// Predict requests an action from the RL inference service for the given observation.
func (c *Client) Predict(ctx context.Context, obs Observation) (*Action, error) {
	body, err := json.Marshal(obs)
	if err != nil {
		return nil, fmt.Errorf("marshal observation: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/predict", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling rl-service /predict: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rl-service /predict returned status %d", resp.StatusCode)
	}

	var action Action
	if err := json.NewDecoder(resp.Body).Decode(&action); err != nil {
		return nil, fmt.Errorf("decode /predict response: %w", err)
	}
	return &action, nil
}
