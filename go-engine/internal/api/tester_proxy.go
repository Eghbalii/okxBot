package api

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// testerHTTPClient is shared across every proxy call — short timeout since this is a
// same-Docker-network hop, not a call across the internet.
var testerHTTPClient = &http.Client{Timeout: 5 * time.Second}

// proxyTesterRequest forwards method+body to cmd/strategy-tester at path and copies its response
// back verbatim (status code, body, content-type) — the tester's JSON is already shaped exactly
// how the panel wants it, so re-decoding/re-encoding here would just be duplicated schema.
func (s *Server) proxyTesterRequest(w http.ResponseWriter, r *http.Request, method, path string) {
	if s.TesterBaseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "strategy-tester is not configured (TESTER_SERVICE_URL unset)")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), method, s.TesterBaseURL+path, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := testerHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("strategy-tester unreachable: %v", err))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// proxyTester and proxyTesterBody are the same shape (proxyTesterRequest already forwards the
// body regardless of method) — kept as two names at the call sites in server.go's route table so
// each route documents its own intent (GET-with-no-body vs. an endpoint that expects one).
func (s *Server) proxyTester(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.proxyTesterRequest(w, r, r.Method, path)
	}
}

func (s *Server) proxyTesterBody(path string) http.HandlerFunc {
	return s.proxyTester(path)
}

func (s *Server) proxyTesterWithID(pathFmt string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.proxyTesterRequest(w, r, r.Method, fmt.Sprintf(pathFmt, r.PathValue("id")))
	}
}

func (s *Server) proxyTesterWithIDBody(pathFmt string) http.HandlerFunc {
	return s.proxyTesterWithID(pathFmt)
}
