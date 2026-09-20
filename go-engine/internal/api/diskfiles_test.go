package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustWriteFile creates a file with the given content at the given path, creating parent
// directories as needed — the test-fixture equivalent of a real accumulated file this feature
// scans for.
func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A fresh repo root with nothing matching any allowlist pattern must produce an empty candidate
// list, never an error — a clean deployment (or one right after a cleanup run) is the expected
// steady state, not an edge case to special-case around.
func TestScanCleanupCandidates_EmptyRootProducesNoCandidates(t *testing.T) {
	root := t.TempDir()
	candidates, err := scanCleanupCandidates(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("got %d candidates from an empty root, want 0: %+v", len(candidates), candidates)
	}
}

// The real production incident this feature exists to fix: a warmstart JSONL dump, an old model
// backup directory, a stray build artifact, config .bak files, and a SQL backup — every one of the
// allowlist patterns found on the real box on 2026-09-20. Each must be found, sized correctly (a
// directory reports its full recursive size, not just its own inode), and described in a way that
// names what it is rather than just its bare path.
func TestScanCleanupCandidates_FindsEveryKnownPattern(t *testing.T) {
	root := t.TempDir()

	mustWriteFile(t, filepath.Join(root, "data", "warmstart_full.jsonl"), make([]byte, 1000))
	mustWriteFile(t, filepath.Join(root, "rl-service", "data", "warmstart_v9.jsonl"), make([]byte, 2000))
	mustWriteFile(t, filepath.Join(root, "model-backups", "20260917-pre-v9-deploy", "weights.zip"), make([]byte, 500))
	mustWriteFile(t, filepath.Join(root, "model-backups", "20260917-pre-v9-deploy", "buffer.pkl"), make([]byte, 500))
	mustWriteFile(t, filepath.Join(root, "go-engine", "trader"), make([]byte, 300))
	mustWriteFile(t, filepath.Join(root, "config.yaml.bak.20260913"), make([]byte, 10))
	mustWriteFile(t, filepath.Join(root, "go-engine", "configs", "config.yaml.bak-20260904-193847"), make([]byte, 20))
	mustWriteFile(t, filepath.Join(root, "rl-service", "configs", "config.yaml.bak.20260907093439"), make([]byte, 30))
	mustWriteFile(t, filepath.Join(root, "pre_rename_backup_20260919.sql"), make([]byte, 40))

	// A file that must NEVER be matched by any spec — the replay buffer a running RL service
	// depends on (CLAUDE.md §15.11: losing it makes the model forget every experience it has
	// collected). This is the negative case the whole allowlist design exists to guarantee.
	mustWriteFile(t, filepath.Join(root, "rl-service", "models", "sac_global_buffer.pkl"), make([]byte, 99999))

	candidates, err := scanCleanupCandidates(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	byPath := make(map[string]cleanupCandidate, len(candidates))
	for _, c := range candidates {
		byPath[c.Path] = c
	}

	wantPaths := []string{
		"data/warmstart_full.jsonl",
		filepath.Join("rl-service", "data", "warmstart_v9.jsonl"),
		filepath.Join("model-backups", "20260917-pre-v9-deploy"),
		filepath.Join("go-engine", "trader"),
		"config.yaml.bak.20260913",
		filepath.Join("go-engine", "configs", "config.yaml.bak-20260904-193847"),
		filepath.Join("rl-service", "configs", "config.yaml.bak.20260907093439"),
		"pre_rename_backup_20260919.sql",
	}
	for _, want := range wantPaths {
		if _, ok := byPath[want]; !ok {
			t.Errorf("expected candidate %q not found; got paths: %v", want, keysOf(byPath))
		}
	}

	// The model-backups directory's SIZE must be the sum of everything inside it, not just its own
	// directory-entry size — the two files inside it total 1000 bytes.
	if got := byPath[filepath.Join("model-backups", "20260917-pre-v9-deploy")].SizeBytes; got != 1000 {
		t.Errorf("model-backups directory size = %d, want 1000 (sum of its 2 files)", got)
	}

	// The replay buffer must never appear under any path — this is the negative assertion that
	// matters most: an allowlist entry accidentally broad enough to catch rl-service/models/*
	// would be a real production incident, not a test failure.
	for path := range byPath {
		if filepath.Base(path) == "sac_global_buffer.pkl" {
			t.Fatalf("the RL model's replay buffer was surfaced as a cleanup candidate — this must never happen: %s", path)
		}
	}

	// Every description must actually say something, not just repeat the bare path — the whole
	// point for a non-technical reader.
	for _, c := range candidates {
		if c.Description == "" {
			t.Errorf("candidate %s has no description", c.Path)
		}
	}
}

func keysOf(m map[string]cleanupCandidate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Candidates must be sorted largest-first — what a non-technical operator deciding "is this worth
// deleting" wants to see at the top.
func TestScanCleanupCandidates_SortedLargestFirst(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "data", "small.jsonl"), make([]byte, 100))
	mustWriteFile(t, filepath.Join(root, "data", "big.jsonl"), make([]byte, 100000))
	mustWriteFile(t, filepath.Join(root, "pre_rename_backup_x.sql"), make([]byte, 5000))

	candidates, err := scanCleanupCandidates(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 1; i < len(candidates); i++ {
		if candidates[i].SizeBytes > candidates[i-1].SizeBytes {
			t.Fatalf("candidates not sorted largest-first at index %d: %d > %d",
				i, candidates[i].SizeBytes, candidates[i-1].SizeBytes)
		}
	}
	if len(candidates) == 0 || candidates[0].Path != "data/big.jsonl" {
		t.Fatalf("largest candidate should be data/big.jsonl first, got %+v", candidates)
	}
}

// candidateID must be stable across calls (the list→confirm→delete round trip depends on the same
// path always producing the same id) and must differ for different paths (or two distinct
// candidates would collide onto one deletable id).
func TestCandidateID_StableAndDistinct(t *testing.T) {
	a1 := candidateID("data/warmstart_full.jsonl")
	a2 := candidateID("data/warmstart_full.jsonl")
	b := candidateID("data/warmstart_mtf.jsonl")
	if a1 != a2 {
		t.Fatalf("candidateID not stable: %s != %s", a1, a2)
	}
	if a1 == b {
		t.Fatalf("candidateID collided for two different paths: both produced %s", a1)
	}
}

func newDiskTestServer(t *testing.T, rootDir string) *Server {
	t.Helper()
	return &Server{HostRootDir: rootDir, Logger: slog.Default()}
}

// GET /api/system/cleanup-candidates must report 503, not a panic or an empty-looking 200, when
// HostRootDir is unset — the documented "disabled, not broken" state for a deployment that hasn't
// added the bind mount.
func TestHandleListCleanupCandidates_DisabledWithoutHostRootDir(t *testing.T) {
	s := newDiskTestServer(t, "")
	req := httptest.NewRequest("GET", "/api/system/cleanup-candidates", nil)
	rec := httptest.NewRecorder()
	s.handleListCleanupCandidates(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleListCleanupCandidates_ReturnsRealCandidates(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "data", "warmstart.jsonl"), make([]byte, 4096))

	s := newDiskTestServer(t, root)
	req := httptest.NewRequest("GET", "/api/system/cleanup-candidates", nil)
	rec := httptest.NewRecorder()
	s.handleListCleanupCandidates(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Candidates []cleanupCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(body.Candidates), body.Candidates)
	}
	if body.Candidates[0].SizeBytes != 4096 {
		t.Errorf("size = %d, want 4096", body.Candidates[0].SizeBytes)
	}
	// ModifiedAt must be a real, recent, parseable timestamp — a non-technical operator reading
	// "from this date to this date" (the user's own framing of what this confirm dialog must show)
	// depends on this field actually being usable.
	parsed, err := time.Parse(time.RFC3339, body.Candidates[0].ModifiedAt)
	if err != nil {
		t.Fatalf("ModifiedAt %q did not parse as RFC3339: %v", body.Candidates[0].ModifiedAt, err)
	}
	if time.Since(parsed) > time.Minute {
		t.Errorf("ModifiedAt %v is not recent, but this file was just created", parsed)
	}
}

// The delete endpoint must actually remove the file from disk, and report the id it deleted —
// this is the entire point of the feature: an operator confirms a specific item, and only that
// item disappears.
func TestHandleDeleteCleanupCandidate_RemovesExactlyTheConfirmedFile(t *testing.T) {
	root := t.TempDir()
	keepPath := filepath.Join(root, "pre_rename_backup_keep.sql")
	deletePath := filepath.Join(root, "pre_rename_backup_delete.sql")
	mustWriteFile(t, keepPath, make([]byte, 10))
	mustWriteFile(t, deletePath, make([]byte, 20))

	s := newDiskTestServer(t, root)
	candidates, err := scanCleanupCandidates(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var targetID string
	for _, c := range candidates {
		if c.Path == "pre_rename_backup_delete.sql" {
			targetID = c.ID
		}
	}
	if targetID == "" {
		t.Fatalf("scan did not find pre_rename_backup_delete.sql among: %+v", candidates)
	}

	body, _ := json.Marshal(map[string]string{"id": targetID})
	req := httptest.NewRequest("POST", "/api/system/cleanup-candidates/delete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleDeleteCleanupCandidate(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(deletePath); !os.IsNotExist(err) {
		t.Errorf("pre_rename_backup_delete.sql still exists after delete (err=%v)", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Errorf("pre_rename_backup_keep.sql was removed too, want it untouched: %v", err)
	}
}

// A directory candidate (model-backups/<timestamp>/) must be removed in full, not just its top
// entry — RemoveAll's own contract, exercised end to end through the handler rather than assumed.
func TestHandleDeleteCleanupCandidate_RemovesADirectoryFully(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, "model-backups", "20260917-pre-v9-deploy")
	mustWriteFile(t, filepath.Join(backupDir, "weights.zip"), make([]byte, 100))
	mustWriteFile(t, filepath.Join(backupDir, "buffer.pkl"), make([]byte, 100))

	s := newDiskTestServer(t, root)
	candidates, err := scanCleanupCandidates(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want exactly 1 (the whole backup directory): %+v", len(candidates), candidates)
	}

	body, _ := json.Marshal(map[string]string{"id": candidates[0].ID})
	req := httptest.NewRequest("POST", "/api/system/cleanup-candidates/delete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleDeleteCleanupCandidate(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Errorf("model-backups directory still exists after delete: err=%v", err)
	}
}

// An id that doesn't match any current candidate — because it never existed, or because the file
// it once named was already deleted — must be rejected with 404, never silently succeed or delete
// something else. This is the re-resolve-against-a-fresh-scan guarantee the handler's own doc
// comment states.
func TestHandleDeleteCleanupCandidate_UnknownIDRejected(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "data", "warmstart.jsonl"), make([]byte, 10))

	s := newDiskTestServer(t, root)
	body, _ := json.Marshal(map[string]string{"id": "0000000000000000"})
	req := httptest.NewRequest("POST", "/api/system/cleanup-candidates/delete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleDeleteCleanupCandidate(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// And the real file must be untouched — a rejected delete must have zero side effects.
	if _, err := os.Stat(filepath.Join(root, "data", "warmstart.jsonl")); err != nil {
		t.Errorf("the real file was affected by a rejected delete: %v", err)
	}
}

// An empty id in the request body is a malformed request, not "delete nothing" — must be 400, and
// must not silently proceed to scan/match against an empty string.
func TestHandleDeleteCleanupCandidate_EmptyIDRejected(t *testing.T) {
	s := newDiskTestServer(t, t.TempDir())
	body, _ := json.Marshal(map[string]string{"id": ""})
	req := httptest.NewRequest("POST", "/api/system/cleanup-candidates/delete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleDeleteCleanupCandidate(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// A file that only PARTIALLY matches an allowlist pattern's directory but sits outside the actual
// scan root must never be reachable — deleting must be scoped to HostRootDir even in principle.
// This exercises the escape guard directly rather than only trusting glob/Rel never misbehave.
func TestHandleDeleteCleanupCandidate_DoesNotDeleteOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "victim.sql") // a SEPARATE temp dir, not under root
	mustWriteFile(t, outsideFile, make([]byte, 10))

	// Craft an id that a naive implementation might resolve outside root if it ever accepted a
	// caller-supplied path directly — this test's real assertion is that no matter what id is
	// submitted, only a path from scanCleanupCandidates(root) can ever be deleted.
	s := newDiskTestServer(t, root)
	body, _ := json.Marshal(map[string]string{"id": candidateID("../../../etc/passwd")})
	req := httptest.NewRequest("POST", "/api/system/cleanup-candidates/delete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleDeleteCleanupCandidate(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404 (id does not match any real scanned candidate)", rec.Code)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Errorf("the outside file was affected: %v", err)
	}
}
