package health_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/health"
)

func TestHTTPCheckerReportsHealthyForSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/up" {
			t.Fatalf("expected /up path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	status, err := health.NewHTTPChecker(server.Client()).Check(context.Background(), health.Target{Address: server.URL, Path: "/up"})

	if err != nil {
		t.Fatalf("check health: %v", err)
	}
	if status != health.Healthy {
		t.Fatalf("expected healthy, got %s", status)
	}
}

func TestHTTPCheckerReportsUnhealthyForFailedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	status, err := health.NewHTTPChecker(server.Client()).Check(context.Background(), health.Target{Address: server.URL, Path: "/up"})

	if err != nil {
		t.Fatalf("check health: %v", err)
	}
	if status != health.Unhealthy {
		t.Fatalf("expected unhealthy, got %s", status)
	}
}

func TestDefaultHTTPCheckerDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
	}))
	defer internal.Close()
	workload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusFound)
	}))
	defer workload.Close()

	status, err := health.NewHTTPChecker(nil).Check(context.Background(), health.Target{Address: workload.URL, Path: "/up"})

	if err != nil {
		t.Fatalf("check health: %v", err)
	}
	if followed.Load() {
		t.Fatal("health probe followed a workload-controlled redirect")
	}
	if status != health.Healthy {
		t.Fatalf("expected 3xx to count as healthy, got %s", status)
	}
}

func TestHTTPClientBoundsHangingProbe(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	done := make(chan health.Status, 1)
	go func() {
		status, _ := health.NewHTTPChecker(health.NewHTTPClient(50*time.Millisecond)).Check(context.Background(), health.Target{Address: server.URL})
		done <- status
	}()

	select {
	case status := <-done:
		if status != health.Unhealthy {
			t.Fatalf("expected hanging probe to be unhealthy, got %s", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("health probe did not time out")
	}
}
