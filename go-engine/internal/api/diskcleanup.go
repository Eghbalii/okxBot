package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// dockerSocket is the path the Docker Engine API is reached on. cmd/api already mounts this
// (docker-compose.yml, read-only — which prevents remounting the bind, not talking to the socket;
// the Engine API is ordinary request/response over it either way).
const dockerSocket = "/var/run/docker.sock"

// cleanupResult reports what a cleanup run reclaimed, so the panel can say what actually happened
// rather than only that something did.
type cleanupResult struct {
	BuildCacheBytes int64  `json:"buildCacheBytes"`
	Error           string `json:"error,omitempty"`
}

// handleDiskCleanup reclaims disk space from the Docker build cache (2026-09-09 request).
//
// Build cache ONLY, and the allowlist is enforced by what this file contains rather than by a
// guard a future edit could relax. It is also the only target that matters in practice: measured
// on this box, a single day of service rebuilds produced 7.28GB of it against a 25GB disk — the
// difference between 85% and 55% full — while every other cache combined was ~250MB.
//
// Deliberately absent, and not to be added:
//   - image pruning: okxbot-trader has no running container between deploys, and was needed hours
//     after exactly such a gap
//   - volume pruning: the database and the RL model's replay buffer live in volumes, and losing
//     the buffer makes the model forget every experience it has collected (CLAUDE.md §15.11)
//   - anything under rl-service/models
//
// A button that could destroy those is a button that eventually will, so the calls simply do not
// exist here — the same posture as §5's risk limits: the unsafe path is unrepresentable, not
// merely discouraged.
//
// apt-get and journalctl are deliberately NOT invoked either, though the manual cleanup uses them:
// both are host tools absent from this Alpine-based image (verified, not assumed), so calling them
// would fail on every run and report an error for something that was never going to work from
// inside a container. They stay a host-side task.
func (s *Server) handleDiskCleanup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var out cleanupResult
	n, err := pruneBuildCache(ctx)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.BuildCacheBytes = n
	}

	if s.Logger != nil {
		s.Logger.Info("disk cleanup run", "buildCacheBytes", out.BuildCacheBytes, "error", out.Error)
	}
	// 200 even on failure: the response body carries the outcome, and the panel needs to render
	// the reason rather than a bare status code.
	writeJSON(w, http.StatusOK, out)
}

// pruneBuildCache calls the Docker Engine API's build-prune endpoint directly over the mounted
// socket. Deliberately not shelling out to a `docker` binary: Dockerfile.api has never installed
// the Docker CLI (which is why this package's own dockerStatus/LogTail do not work either,
// CLAUDE.md §18), and adding a CLI to an image just to make one HTTP call would be the wrong trade.
func pruneBuildCache(ctx context.Context) (int64, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", dockerSocket)
			},
		},
	}
	// all=true prunes unused cache regardless of age, matching `docker builder prune -af` — the
	// command that reclaimed 7.28GB by hand. Without it only dangling entries go, which on this box
	// left most of the cache in place.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/v1.41/build/prune?all=true", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("docker socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("docker build prune returned %s", resp.Status)
	}
	var body struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode prune response: %w", err)
	}
	return body.SpaceReclaimed, nil
}
