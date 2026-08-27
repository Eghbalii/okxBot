package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

// OpenTrial is one disposable, TTL'd "trial position" for a candidate parameter set (CLAUDE.md
// §16.3 step 2) — deliberately not a paper_orders row: these are throwaway experiments, not real
// or even real-paper trades. Stored in Redis, not Postgres, per the explicit "disposable state"
// decision.
type OpenTrial struct {
	StudyID  string           `json:"studyId"`
	TrialID  int              `json:"trialId"`
	InstID   string           `json:"instId"`
	Side     string           `json:"side"` // "buy" or "sell"
	EntryPx  decimal.Decimal  `json:"entryPx"`
	SLPx     *decimal.Decimal `json:"slPx,omitempty"`
	TPPx     *decimal.Decimal `json:"tpPx,omitempty"`
	OpenedAt time.Time        `json:"openedAt"`
}

// TrialStore persists OpenTrial state in Redis with a TTL (CLAUDE.md §16.3 step 2: "TTL should
// exceed the run's remaining time box comfortably ... so stale trial state self-cleans if the
// process crashes"). Reuses the existing github.com/redis/go-redis/v9 client — no second Redis
// library.
type TrialStore struct {
	rdb *redis.Client
}

// NewTrialStore creates a TrialStore against the given Redis address.
func NewTrialStore(addr string) *TrialStore {
	return &TrialStore{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

// CloseConn closes the underlying Redis connection (named to avoid colliding with the per-trial
// Close method below).
func (s *TrialStore) CloseConn() error { return s.rdb.Close() }

// key is namespaced per (run, inst_id) so listing open trials for a tick-check pass
// (OpenForInst) doesn't have to scan across unrelated runs/instruments — trial ids are unique
// within a run's candidate set, but namespacing avoids any cross-run key collision entirely.
func trialKey(runID, instID string, trialID int) string {
	return fmt.Sprintf("optimizer:trial:%s:%s:%d", runID, instID, trialID)
}

// trialIndexKey is a Redis Set of trial keys currently open for (runID, instID) — used so
// OpenForInst doesn't need a Redis KEYS/SCAN (slow, and discouraged in production Redis) to find
// them; membership is added on Open and removed on Close.
func trialIndexKey(runID, instID string) string {
	return fmt.Sprintf("optimizer:trial-index:%s:%s", runID, instID)
}

// Open records a new trial position, TTL'd at ttl.
func (s *TrialStore) Open(ctx context.Context, runID string, t OpenTrial, ttl time.Duration) error {
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal trial: %w", err)
	}
	key := trialKey(runID, t.InstID, t.TrialID)
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, key, b, ttl)
	pipe.SAdd(ctx, trialIndexKey(runID, t.InstID), key)
	pipe.Expire(ctx, trialIndexKey(runID, t.InstID), ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("open trial %s/%d: %w", t.InstID, t.TrialID, err)
	}
	return nil
}

// OpenForInst returns every currently-open trial for (runID, instID) — the tick-driven SL/TP
// touch check (CLAUDE.md §16.3 step 3) calls this on every price tick.
func (s *TrialStore) OpenForInst(ctx context.Context, runID, instID string) ([]OpenTrial, error) {
	keys, err := s.rdb.SMembers(ctx, trialIndexKey(runID, instID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list trial index %s/%s: %w", runID, instID, err)
	}
	if len(keys) == 0 {
		return nil, nil
	}

	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("mget trials %s/%s: %w", runID, instID, err)
	}

	var out []OpenTrial
	var expiredKeys []string
	for i, v := range vals {
		if v == nil {
			// Key expired (TTL) or was deleted without the index entry being cleaned up yet —
			// prune the stale index entry so it doesn't accumulate forever.
			expiredKeys = append(expiredKeys, keys[i])
			continue
		}
		str, ok := v.(string)
		if !ok {
			continue
		}
		var t OpenTrial
		if err := json.Unmarshal([]byte(str), &t); err != nil {
			continue // corrupt entry; skip rather than fail the whole tick-check pass
		}
		out = append(out, t)
	}
	if len(expiredKeys) > 0 {
		s.rdb.SRem(ctx, trialIndexKey(runID, instID), expiredKeys)
	}
	return out, nil
}

// Close removes a trial's state (CLAUDE.md §16.3 step 4: "delete the Redis trial key" on
// SL/TP touch).
func (s *TrialStore) Close(ctx context.Context, runID string, t OpenTrial) error {
	key := trialKey(runID, t.InstID, t.TrialID)
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, key)
	pipe.SRem(ctx, trialIndexKey(runID, t.InstID), key)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("close trial %s/%d: %w", t.InstID, t.TrialID, err)
	}
	return nil
}
