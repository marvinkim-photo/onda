package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const readinessCheckTimeout = 2 * time.Second

type readinessCheck func(context.Context) error

type componentStatus string

const (
	statusPending     componentStatus = "pending"
	statusReady       componentStatus = "ready"
	statusFailed      componentStatus = "failed"
	statusUnavailable componentStatus = "unavailable"
)

type readinessProbe struct {
	mu         sync.RWMutex
	checks     map[string]readinessCheck
	components map[string]componentStatus
}

type healthResponse struct {
	OK         bool                       `json:"ok"`
	Components map[string]componentStatus `json:"components,omitempty"`
}

func newReadinessProbe(checks map[string]readinessCheck, requiredComponents []string) *readinessProbe {
	components := make(map[string]componentStatus, len(requiredComponents))
	for _, name := range requiredComponents {
		components[name] = statusPending
	}
	return &readinessProbe{checks: checks, components: components}
}

func (p *readinessProbe) markReady(name string) {
	p.setStatus(name, statusReady)
}

func (p *readinessProbe) markFailed(name string) {
	p.setStatus(name, statusFailed)
}

func (p *readinessProbe) setStatus(name string, status componentStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, required := p.components[name]; required {
		p.components[name] = status
	}
}

func (p *readinessProbe) evaluate(ctx context.Context) healthResponse {
	p.mu.RLock()
	statuses := make(map[string]componentStatus, len(p.components)+len(p.checks))
	for name, status := range p.components {
		statuses[name] = status
	}
	p.mu.RUnlock()

	checkCtx, cancel := context.WithTimeout(ctx, readinessCheckTimeout)
	defer cancel()

	var wg sync.WaitGroup
	var statusMu sync.Mutex
	for name, check := range p.checks {
		name, check := name, check
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := statusReady
			if err := check(checkCtx); err != nil {
				status = statusUnavailable
			}
			statusMu.Lock()
			statuses[name] = status
			statusMu.Unlock()
		}()
	}
	wg.Wait()

	ok := true
	for _, status := range statuses {
		if status != statusReady {
			ok = false
			break
		}
	}
	return healthResponse{OK: ok, Components: statuses}
}

func healthHandler(probe *readinessProbe) http.Handler {
	r := chi.NewRouter()
	livez := func(w http.ResponseWriter, _ *http.Request) {
		writeHealthJSON(w, http.StatusOK, healthResponse{OK: true})
	}
	r.Get("/livez", livez)
	// Backward-compatible liveness alias for existing Compose health checks.
	r.Get("/healthz", livez)
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		response := probe.evaluate(req.Context())
		status := http.StatusOK
		if !response.OK {
			status = http.StatusServiceUnavailable
		}
		writeHealthJSON(w, status, response)
	})
	r.Handle("/metrics", promhttp.Handler())
	return r
}

func writeHealthJSON(w http.ResponseWriter, status int, response healthResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func serveHealth(ctx context.Context, addr string, logger *slog.Logger, probe *readinessProbe) error {
	srv := &http.Server{Addr: addr, Handler: healthHandler(probe)}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	logger.Info("health 리슨", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
