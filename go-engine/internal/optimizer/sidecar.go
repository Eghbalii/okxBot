package optimizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// SidecarClient calls the Python/Optuna candidate-proposal sidecar (optimizer-service/, kept
// as-is from the original design — its ask/tell protocol was never the problem, only how the two
// removed Go services drove it, CLAUDE.md §21/§33.5). Mirrors internal/rlclient.Client's shape
// (BaseURL + plain net/http, JSON body, no retries in v1).
type SidecarClient struct {
	BaseURL    string
	httpClient *http.Client
}

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

// SuggestedCandidate is one proposed parameter set from the sidecar, keyed by its opaque TrialID
// (the sidecar's Optuna trial number) so a later Report call attributes the outcome to the right
// underlying optuna.Trial.
type SuggestedCandidate struct {
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
// ranges. Each returned candidate's params are clamped to specs' own ranges before being handed
// back, so a sidecar rounding quirk can never propose a value outside what the strategy itself
// declared as valid.
func (c *SidecarClient) Suggest(ctx context.Context, studyID string, specs []SidecarParamSpec, n int) ([]SuggestedCandidate, error) {
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

	specByName := make(map[string]strategy.ParamSpec, len(specs))
	for _, s := range specs {
		specByName[s.Name] = strategy.ParamSpec{Name: s.Name, Min: decimal.NewFromFloat(s.Min), Max: decimal.NewFromFloat(s.Max)}
	}

	candidates := make([]SuggestedCandidate, 0, len(out.Candidates))
	for _, rc := range out.Candidates {
		params := make(map[string]decimal.Decimal, len(rc.Params))
		for k, v := range rc.Params {
			d := decimal.NewFromFloat(v)
			if spec, ok := specByName[k]; ok {
				d = strategy.ClampParam(spec, d)
			}
			params[k] = d
		}
		candidates = append(candidates, SuggestedCandidate{TrialID: rc.TrialID, Params: params})
	}
	return candidates, nil
}

type reportRequest struct {
	StudyID string  `json:"study_id"`
	TrialID int     `json:"trial_id"`
	Score   float64 `json:"score"`
}

// Report tells the sidecar how one candidate scored, closing the loop for its next suggestion.
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

// StudyID builds this pipeline's Optuna study id — scoped to the FULL lineage (kind, inst_id,
// bar, exchange, risk_profile), not just "{inst_id}:{kind}" like the removed
// cmd/strategy-optimizer's convention, since the same kind+token pair can now be tuned
// independently under two different risk profiles/exchanges (the operator's explicit two-track
// OKX-10x/MEXC-100x split) and each needs its own, non-colliding Optuna study.
func StudyID(l Lineage) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s", l.Exchange, l.RiskProfile, l.InstID, l.Bar, l.Kind)
}
