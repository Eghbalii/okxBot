package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Talks to Docker's Engine API over its unix socket directly, rather than shelling out to a
// `docker` binary.
//
// procstatus.go's dockerStatus does shell out, and CLAUDE.md §18 records the consequence: the CLI
// was never added to Dockerfile.api, so every one of those calls has silently failed since it was
// written. The socket is mounted and working (verified 2026-09-13); only the binary was missing.
// Dialing it from Go removes the dependency entirely instead of adding a ~50MB CLI to the image to
// run one command.
//
// Read-only by construction: nothing here can start, stop or modify a container. The socket is
// effectively root on the host, so keeping this surface to GET requests matters — a bug here must
// not be able to do anything to the machine.

// newDockerHTTPClient returns an HTTP client whose transport dials the unix socket. The "host" in
// the URL is ignored by the socket, so any placeholder works.
func newDockerHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

// ContainerState is one container's current state, as Docker reports it.
type ContainerState struct {
	Name string `json:"name"`
	// State is Docker's own vocabulary: "running", "restarting", "exited", "created", "paused".
	// Passed through unchanged rather than mapped to a local set — "restarting" in particular is
	// the signal that distinguishes a crash loop (CLAUDE.md §47) from a healthy service, and
	// flattening it into "down" would erase exactly the distinction that mattered most.
	State string `json:"state"`
	// Status is the human string ("Up 10 minutes", "Restarting (1) 20 seconds ago"), which carries
	// the uptime and restart count that make a crash loop obvious at a glance.
	Status string `json:"status"`
	// Health is the container's healthcheck verdict when it declares one, empty otherwise.
	Health string `json:"health,omitempty"`
}

// dockerContainer is the subset of Docker's /containers/json entry this needs.
type dockerContainer struct {
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
	// Health is an OBJECT here — {"Status":"healthy","FailingStreak":0} — not the string its name
	// suggests (verified against the live socket, 2026-09-13). Decoding it as a string fails the
	// WHOLE response, which on first deploy blanked every service's state at once rather than just
	// this one field. A field list is not a schema.
	Health struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

// listContainers returns every container's state, keyed by its name with Docker's leading slash
// stripped.
//
// Includes stopped containers (all=1) deliberately: a service that has exited is precisely what
// this is for, and omitting it would make a dead service look identical to one that was never
// configured.
func listContainers(ctx context.Context, client *http.Client) (map[string]ContainerState, error) {
	return listContainersFrom(ctx, client, "http://docker/containers/json?all=1")
}

// listContainersFrom is listContainers with the URL injected, so the decode path can be exercised
// against Docker's real response shape without a unix socket.
func listContainersFrom(ctx context.Context, client *http.Client, url string) (map[string]ContainerState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build docker request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query docker socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker api returned %d", resp.StatusCode)
	}

	var raw []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode docker response: %w", err)
	}

	out := make(map[string]ContainerState, len(raw))
	for _, c := range raw {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		// Docker reports "none" for a container that declares no healthcheck. Normalized to empty
		// so the panel shows nothing rather than a badge reading "none", which looks like a
		// failed check rather than an absent one.
		health := c.Health.Status
		if health == "none" {
			health = ""
		}
		out[name] = ContainerState{Name: name, State: c.State, Status: c.Status, Health: health}
	}
	return out, nil
}
