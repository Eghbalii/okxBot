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
	ImagesBytes     int64  `json:"imagesBytes"`
	ContainersBytes int64  `json:"containersBytes"`
	Error           string `json:"error,omitempty"`
}

// handleDiskCleanup reclaims disk space from three Docker-internal sources, fully automatically —
// no confirmation, because every one of these is safe by Docker's own construction, not merely by
// convention (2026-09-20 redesign, after the original build-cache-only version was found to
// reclaim almost nothing on a real box: measured at 20GB/25GB used with the build cache already
// empty, while three stopped containers alone (from old deploys, never running again) were pinning
// ~3GB of otherwise-unused images. The real disk hog turned out to be a *different* thing entirely
// — a Kafka topic's retention misconfiguration, fixed separately in internal/kafkastream — but this
// button's own job is still worth doing properly:
//
//   - build cache (build/prune?all=true, unchanged from the original version)
//   - stopped/exited containers (containers/prune) — Docker will never remove a RUNNING container
//     via this call; it is defined to only ever touch containers already in the "exited" or
//     "created" state, so there is no path from calling this to interrupting a live service
//   - images with zero containers (running OR stopped) referencing them
//     (images/prune?filters={"dangling":["false"]}, i.e. Docker's own `docker image prune -a`
//     semantics) — an image every remaining container still uses can never be selected by this
//     filter, so a service's own currently-running image is structurally unreachable by this call
//
// Deliberately absent, and not to be added:
//   - volume pruning: the database and the RL model's replay buffer live there — losing the buffer
//     makes the model forget every experience it has collected (CLAUDE.md §15.11)
//   - anything that could touch a path a service actually reads from while running
//
// A button that could destroy those is a button that eventually will, so the calls simply do not
// exist here — the same posture as §5's risk limits: the unsafe path is unrepresentable, not
// merely discouraged. Large accumulated FILES outside Docker's own accounting (training-data
// dumps, old model backups, stray build artifacts) are a separate, confirm-per-item flow —
// diskfiles.go — since deleting those is a real, judgment-requiring choice a non-technical
// operator should see and approve, not something to fold into an automatic button.
//
// apt-get and journalctl are deliberately NOT invoked either, though the manual cleanup uses them:
// both are host tools absent from this Alpine-based image (verified, not assumed), so calling them
// would fail on every run and report an error for something that was never going to work from
// inside a container. They stay a host-side task.
func (s *Server) handleDiskCleanup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var out cleanupResult
	var errs []string

	if n, err := pruneBuildCache(ctx); err != nil {
		errs = append(errs, err.Error())
	} else {
		out.BuildCacheBytes = n
	}
	// Containers before images: a stopped container is what pins an otherwise-unused image in
	// place (docker system df -v shows 0 CONTAINERS only once the container referencing it is
	// gone) — pruning in the other order would leave the images/prune call finding nothing to do
	// on its first run after a batch of old deploys, requiring a second click to actually reclaim
	// what the stopped containers were holding onto.
	if n, err := pruneContainers(ctx); err != nil {
		errs = append(errs, err.Error())
	} else {
		out.ContainersBytes = n
	}
	if n, err := pruneUnusedImages(ctx); err != nil {
		errs = append(errs, err.Error())
	} else {
		out.ImagesBytes = n
	}

	if len(errs) > 0 {
		out.Error = fmt.Sprintf("%d of 3 cleanup steps failed: %v", len(errs), errs)
	}

	if s.Logger != nil {
		s.Logger.Info("disk cleanup run",
			"buildCacheBytes", out.BuildCacheBytes,
			"containersBytes", out.ContainersBytes,
			"imagesBytes", out.ImagesBytes,
			"error", out.Error)
	}
	// 200 even on partial failure: the response body carries the outcome, and the panel needs to
	// render what DID succeed rather than a bare status code that would hide a real partial win.
	writeJSON(w, http.StatusOK, out)
}

// dockerPost calls a Docker Engine API POST endpoint over the mounted socket and decodes a
// {"SpaceReclaimed": N} response — the shape every prune endpoint used here shares. Deliberately
// not shelling out to a `docker` binary: Dockerfile.api has never installed the Docker CLI (which
// is why this package's own dockerStatus/LogTail do not work either, CLAUDE.md §18), and adding a
// CLI to an image just to make a few HTTP calls would be the wrong trade.
func dockerPost(ctx context.Context, path string) (int64, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", dockerSocket)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("docker socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("docker call to %s returned %s", path, resp.Status)
	}
	var body struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode response from %s: %w", path, err)
	}
	return body.SpaceReclaimed, nil
}

// pruneBuildCache reclaims the Docker build cache. all=true prunes unused cache regardless of age,
// matching `docker builder prune -af` — the command that reclaimed 7.28GB by hand when this button
// was first built. Without it only dangling entries go, which on this box left most of the cache
// in place.
func pruneBuildCache(ctx context.Context) (int64, error) {
	return dockerPost(ctx, "/v1.41/build/prune?all=true")
}

// pruneContainers removes stopped/exited containers only. Docker's own definition of this
// endpoint: it selects containers in the "exited" or "created" state — a running container is
// never a candidate, so there is no filter to get wrong here the way there would be with a
// hand-rolled "is this container safe to remove" check.
func pruneContainers(ctx context.Context) (int64, error) {
	return dockerPost(ctx, "/v1.41/containers/prune")
}

// pruneUnusedImages removes every image with zero containers (running or stopped) referencing it
// — the dangling=false filter is Docker's own `docker image prune -a` semantics, not merely
// "untagged" images. This is what actually reclaims space after a batch of old service rebuilds:
// an old okxbot-trader/okxbot-optimizer-service image left behind by a previous deploy has a real
// tag and would never be touched by the narrower dangling=true filter, but once nothing runs it
// it's pure waste. An image any remaining container still depends on can never match this filter,
// by Docker's own accounting — this call cannot remove an image a live service needs.
func pruneUnusedImages(ctx context.Context) (int64, error) {
	return dockerPost(ctx, `/v1.41/images/prune?filters=%7B%22dangling%22%3A%5B%22false%22%5D%7D`)
}
