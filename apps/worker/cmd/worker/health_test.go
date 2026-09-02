package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestHealthHandlerLivenessIsIndependentOfReadiness(t *testing.T) {
	probe := newReadinessProbe(map[string]readinessCheck{
		"postgres": func(context.Context) error { return errors.New("database unavailable") },
	}, []string{"consumer_ingest"})
	handler := healthHandler(probe)

	for _, path := range []string{"/livez", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if !strings.Contains(response.Body.String(), `"ok":true`) {
				t.Fatalf("body = %q, want live response", response.Body.String())
			}
		})
	}
}

func TestHealthHandlerReadyWhenStoresAndComponentsAreReady(t *testing.T) {
	probe := newReadinessProbe(map[string]readinessCheck{
		"postgres": func(context.Context) error { return nil },
		"redis":    func(context.Context) error { return nil },
	}, []string{"consumer_ingest"})
	probe.markReady("consumer_ingest")

	response := httptest.NewRecorder()
	healthHandler(probe).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"consumer_ingest":"ready"`) {
		t.Fatalf("body = %q, want ready component", response.Body.String())
	}
}

func TestHealthHandlerReadinessFailureIsRedacted(t *testing.T) {
	const secret = "postgres://onda:do-not-leak@database.internal/onda"
	probe := newReadinessProbe(map[string]readinessCheck{
		"postgres": func(context.Context) error { return errors.New(secret) },
	}, []string{"consumer_ingest"})

	response := httptest.NewRecorder()
	healthHandler(probe).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, body)
	}
	if strings.Contains(string(body), secret) || strings.Contains(string(body), "do-not-leak") {
		t.Fatalf("readiness response leaked check error: %s", body)
	}
	for _, want := range []string{`"postgres":"unavailable"`, `"consumer_ingest":"pending"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("body = %q, want %s", body, want)
		}
	}
}

func TestStartWorkerComponentKeepsReadinessFailedDuringRetry(t *testing.T) {
	probe := newReadinessProbe(nil, []string{"consumer_ingest"})
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, ctx := errgroup.WithContext(rootCtx)
	var ran atomic.Bool
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	startWorkerComponent(g, ctx, probe, logger, workerComponent{
		name: "consumer_ingest",
		initialize: func(context.Context) error {
			return errors.New("xgroup failed")
		},
		run: func(context.Context) error {
			ran.Store(true)
			return nil
		},
	})

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for probe.evaluate(context.Background()).Components["consumer_ingest"] != statusFailed {
		select {
		case <-deadline.C:
			t.Fatal("component did not enter failed readiness state")
		case <-ticker.C:
		}
	}

	if ran.Load() {
		t.Fatal("component ran after initialization failure")
	}
	response := httptest.NewRecorder()
	healthHandler(probe).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"consumer_ingest":"failed"`) {
		t.Fatalf("body = %q, want failed consumer status", response.Body.String())
	}

	cancel()
	if err := g.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error = %v, want context canceled", err)
	}
}
