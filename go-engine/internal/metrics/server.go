package metrics

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Serve starts a background HTTP server exposing /metrics for Prometheus to scrape. Errors are
// logged, not returned, since a metrics endpoint failing to bind shouldn't take down the process.
func Serve(addr string, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			logger.Error("metrics server stopped", "addr", addr, "error", err)
		}
	}()
	logger.Info("serving prometheus metrics", "addr", addr)
}
