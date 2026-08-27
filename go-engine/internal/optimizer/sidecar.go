package optimizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
)

// SidecarClient calls the Python/Optuna candidate-proposal sidecar (optimizer-service/,
// CLAUDE.md §16.2) — mirrors internal/rlclient.Client's shape (BaseURL + plain net/http, JSON
// body, no retries in v1).
type SidecarClient struct {
	BaseURL    string
	httpClient *http.Client
}

// NewSidecarClient creates a SidecarClient pointed at the sidecar's base URL.
func NewSidecarClient(baseURL string) *SidecarClient {
	return &SidecarClient{
		BaseURL:    baseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SidecarParamSpec mirrors optimizer_service.api.ParamSpec's JSON shape.
type SidecarParamSpec struct {
	Name string  `json:"name"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// Candidate is one proposed parameter set from the sidecar, keyed by its opaque TrialID (the
// sidecar's Optuna trial number) so a later Report call can attribute the outcome to the right
// underlying optuna.Trial.
type Candidate struct {
	TrialID int
	Params  map[string]decimal.Decimal
}

type suggestRequest struct {
	StudyID     string             `json:"study_id"`
	ParamSpecs  []SidecarParamSpec `json:"param_specs"`
	NCandidates int                `json:"n_candidates"`
}

type suggestResponseCandidate struct {
	TrialID int                `json:"trial_id"`
	Params  map[string]float64 `json:"params"`
}

type suggestResponse struct {
	StudyID    string                     `json:"study_id"`
	Candidates []suggestResponseCandidate `json:"candidates"`
}

// Suggest asks the sidecar for n candidate parameter sets for studyID, given specs' [min,max]
// ranges (CLAUDE.md §16.3 step 1).
func (c *SidecarClient) Suggest(ctx context.Context, studyID string, specs []SidecarParamSpec, n int) ([]Candidate, error) {
	body, err := json.Marshal(suggestRequest{StudyID: studyID, ParamSpecs: specs, NCandidates: n})
	if err != nil {
		return nil, fmt.Errorf("marshal suggest request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/suggest", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build suggest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling optimizer-service /suggest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("optimizer-service /suggest returned status %d", resp.StatusCode)
	}

	var out suggestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode /suggest response: %w", err)
	}

	candidates := make([]Candidate, 0, len(out.Candidates))
	for _, rc := range out.Candidates {
		params := make(map[string]decimal.Decimal, len(rc.Params))
		for k, v := range rc.Params {
			params[k] = decimal.NewFromFloat(v)
		}
		candidates = append(candidates, Candidate{TrialID: rc.TrialID, Params: params})
	}
	return candidates, nil
}

type reportRequest struct {
	StudyID string  `json:"study_id"`
	TrialID int     `json:"trial_id"`
	Score   float64 `json:"score"`
}

// Report tells the sidecar how one candidate's accumulated trades scored (CLAUDE.md §16.3 step
// 4), closing the loop for its next suggestion.
func (c *SidecarClient) Report(ctx context.Context, studyID string, trialID int, score float64) error {
	body, err := json.Marshal(reportRequest{StudyID: studyID, TrialID: trialID, Score: score})
	if err != nil {
		return fmt.Errorf("marshal report request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/report", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling optimizer-service /report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("optimizer-service /report returned status %d", resp.StatusCode)
	}
	return nil
}

// StudyID builds the sidecar study id convention (CLAUDE.md §16.2: `"{inst_id}:{kind}"`).
func StudyID(instID, kind string) string {
	return instID + ":" + kind
}
