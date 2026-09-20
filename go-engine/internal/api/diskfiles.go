package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// cleanupCandidate describes one file or directory a non-technical operator can choose to delete
// from the panel, with enough context to make that choice without needing to know what any of
// these paths are for — the whole point of this endpoint, per the 2026-09-20 request that the
// existing (Docker-only) cleanup button gave a non-programmer nothing to act on for the actual
// bulk of accumulated disk usage.
type cleanupCandidate struct {
	ID          string `json:"id"`
	Path        string `json:"path"`        // relative to the repo root, for display only
	Description string `json:"description"` // plain-language: what this is and why it's safe to remove
	SizeBytes   int64  `json:"sizeBytes"`
	ModifiedAt  string `json:"modifiedAt"` // RFC3339; the file/dir's own mtime
}

// candidateSpec is one entry in the fixed allowlist diskCandidateSpecs enumerates. Every spec
// describes a *pattern* under the repo root, never an operator-suppliable path — the same
// "the allowlist is enforced by what this file contains" posture as diskcleanup.go's Docker prune
// calls, applied here to the filesystem instead of the Docker API.
type candidateSpec struct {
	// globs are relative to the repo root (filepath.Glob patterns); every match becomes one
	// candidate. A glob that matches a directory reports that directory's own total size.
	glob string
	// describe renders the plain-language description for a match, given its relative path.
	describe func(relPath string) string
}

// diskCandidateSpecs is the complete, hardcoded set of file/directory patterns this endpoint will
// ever surface or delete. Nothing outside this list is reachable through either handler below —
// deliberately narrower than "anything large," which would eventually surface (or delete) a
// service's own live data. Found by investigating a real box on 2026-09-20 (CLAUDE.md's own
// disk-cleanup incident): these are exactly the files that had accumulated there with nothing to
// clean them up, none of them read by any running service.
var diskCandidateSpecs = []candidateSpec{
	{
		glob: "data/*.jsonl",
		describe: func(rel string) string {
			return fmt.Sprintf("Training data export (%s) — a one-time snapshot used to warm-start "+
				"the RL model. Not read by any running service; safe to remove once the model has "+
				"trained past the point this was captured for.", filepath.Base(rel))
		},
	},
	{
		glob: "rl-service/data/*.jsonl",
		describe: func(rel string) string {
			return fmt.Sprintf("Training data export (%s) — a one-time snapshot used to warm-start "+
				"the RL model. Not read by any running service; safe to remove once the model has "+
				"trained past the point this was captured for.", filepath.Base(rel))
		},
	},
	{
		glob: "model-backups/*",
		describe: func(rel string) string {
			return fmt.Sprintf("RL model backup (%s) — a saved copy of the trading model's weights "+
				"and learning history, taken before a risky change. Keeping recent ones is a good "+
				"idea; older ones can be removed once you're confident the model is working well.",
				filepath.Base(rel))
		},
	},
	{
		glob: "go-engine/trader",
		describe: func(rel string) string {
			return "A leftover compiled program left behind by a manual build outside Docker. " +
				"The real trading service does not run from this file — it's dead weight."
		},
	},
	{
		glob: "*.bak.*",
		describe: func(rel string) string {
			return fmt.Sprintf("A backup copy of a configuration file (%s), made automatically "+
				"before a change. Safe to remove once you're confident that change worked.", rel)
		},
	},
	{
		glob: "*.bak-*",
		describe: func(rel string) string {
			return fmt.Sprintf("A backup copy of a configuration file (%s), made automatically "+
				"before a change. Safe to remove once you're confident that change worked.", rel)
		},
	},
	{
		glob: "go-engine/configs/*.bak*",
		describe: func(rel string) string {
			return fmt.Sprintf("A backup copy of a configuration file (%s), made automatically "+
				"before a change. Safe to remove once you're confident that change worked.", rel)
		},
	},
	{
		glob: "rl-service/configs/*.bak*",
		describe: func(rel string) string {
			return fmt.Sprintf("A backup copy of a configuration file (%s), made automatically "+
				"before a change. Safe to remove once you're confident that change worked.", rel)
		},
	},
	{
		glob: "*_backup_*.sql",
		describe: func(rel string) string {
			return fmt.Sprintf("A one-time database backup (%s), taken automatically before a "+
				"risky database change. Safe to remove once you're confident that change worked.", rel)
		},
	},
}

// candidateID derives a stable, opaque id from a candidate's relative path — used so the delete
// endpoint only ever accepts an id this scan itself produced, never a raw path from the client.
// A raw-path delete endpoint would need to re-implement this entire allowlist as a validation step
// anyway, at which point it's simpler and safer to just not accept a path at all.
func candidateID(relPath string) string {
	sum := sha256.Sum256([]byte(relPath))
	return hex.EncodeToString(sum[:])[:16]
}

// dirSize sums the size of every regular file under root (recursively) — used for a glob match
// that turns out to be a directory (e.g. one model-backups/<timestamp>/ snapshot).
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// scanCleanupCandidates walks every spec in diskCandidateSpecs against rootDir and returns what it
// finds, largest first — largest-first because that's the order a non-technical operator actually
// cares about ("what's actually worth deleting"), not an arbitrary scan order.
func scanCleanupCandidates(rootDir string) ([]cleanupCandidate, error) {
	var out []cleanupCandidate
	for _, spec := range diskCandidateSpecs {
		matches, err := filepath.Glob(filepath.Join(rootDir, spec.glob))
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", spec.glob, err)
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				continue // vanished between glob and stat — not an error, just skip it
			}
			size := info.Size()
			if info.IsDir() {
				size, err = dirSize(m)
				if err != nil {
					continue
				}
			}
			rel, err := filepath.Rel(rootDir, m)
			if err != nil {
				continue
			}
			out = append(out, cleanupCandidate{
				ID:          candidateID(rel),
				Path:        rel,
				Description: spec.describe(rel),
				SizeBytes:   size,
				ModifiedAt:  info.ModTime().UTC().Format(time.RFC3339),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SizeBytes > out[j].SizeBytes })
	return out, nil
}

// handleListCleanupCandidates lists every file/directory this deployment could confirm-delete
// right now — GET /api/system/cleanup-candidates. Read-only: this handler never deletes anything.
func (s *Server) handleListCleanupCandidates(w http.ResponseWriter, r *http.Request) {
	if s.HostRootDir == "" {
		writeError(w, http.StatusServiceUnavailable, "file cleanup is not available on this deployment")
		return
	}
	candidates, err := scanCleanupCandidates(s.HostRootDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if candidates == nil {
		candidates = []cleanupCandidate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": candidates})
}

// handleDeleteCleanupCandidate deletes exactly one candidate by the id an earlier list call
// returned — POST /api/system/cleanup-candidates/delete, body {"id": "..."}. The id is re-resolved
// against a FRESH scan rather than trusted from the request, so a candidate that no longer exists,
// or a path some other allowlist entry no longer matches, cannot be deleted just because a stale id
// was submitted — a delete can only ever remove something this scan would list right now.
func (s *Server) handleDeleteCleanupCandidate(w http.ResponseWriter, r *http.Request) {
	if s.HostRootDir == "" {
		writeError(w, http.StatusServiceUnavailable, "file cleanup is not available on this deployment")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	candidates, err := scanCleanupCandidates(s.HostRootDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var target *cleanupCandidate
	for i := range candidates {
		if candidates[i].ID == body.ID {
			target = &candidates[i]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "no cleanup candidate with that id — it may have already been removed")
		return
	}

	full := filepath.Join(s.HostRootDir, target.Path)
	// Defense in depth against the id-reuse trust above: re-confirm the resolved path is still
	// inside rootDir before removing anything. filepath.Join with a Rel-derived, allowlist-matched
	// path cannot actually escape rootDir today, but this makes that guarantee explicit rather than
	// resting entirely on filepath.Glob/Rel never being able to produce a "../" — the same
	// belt-and-suspenders posture CLAUDE.md's own safety layers use throughout (e.g. §19.2's three
	// independent SL-cap enforcement points).
	if rel, err := filepath.Rel(s.HostRootDir, full); err != nil || len(rel) >= 2 && rel[:2] == ".." {
		writeError(w, http.StatusInternalServerError, "resolved path escaped the allowed root — refusing to delete")
		return
	}

	if err := os.RemoveAll(full); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to delete %s: %v", target.Path, err))
		return
	}
	if s.Logger != nil {
		s.Logger.Info("cleanup candidate deleted", "path", target.Path, "sizeBytes", target.SizeBytes)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": target.Path, "sizeBytes": target.SizeBytes})
}
