package daemon

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestDestructiveAPIRejectsAmbiguousRequests(t *testing.T) {
	for _, path := range []string{"/v1/remove", "/v1/prune"} {
		for _, body := range []string{`{}`, `{"force":true,"servcie":"typo"}`, `{"force":true} {"force":false}`} {
			t.Run(path+body, func(t *testing.T) {
				rt := fake.NewRuntime()
				d := New(Config{Runtime: rt, StateDir: t.TempDir()})
				w := httptest.NewRecorder()
				d.handler().ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewBufferString(body)))
				if w.Code != 400 {
					t.Fatalf("expected 400, got %d: %s", w.Code, w.Body)
				}
				if len(rt.Operations()) != 0 {
					t.Fatalf("invalid request touched Docker: %v", rt.Operations())
				}
			})
		}
	}
}

func TestPruneRefreshesActualStateWithoutLosingRollback(t *testing.T) {
	rt := fake.NewRuntime()
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
	putSerial(t, d, serialDesired("app", "v1"))
	putSerial(t, d, serialDesired("app", "v2"))
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/prune", bytes.NewBufferString(`{"force":true}`)))
	if w.Code != 200 {
		t.Fatalf("prune: %d %s", w.Code, w.Body)
	}
	actual, err := d.store.LoadActual("app", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.Containers) != 1 || actual.Containers[0].Version != "v2" {
		t.Fatalf("actual state includes pruned containers: %#v", actual)
	}
	lastGood, err := d.store.LoadLastGood("app", "production")
	if err != nil || lastGood.Version != "v1" {
		t.Fatalf("prune lost rollback configuration: %#v %v", lastGood, err)
	}
}

func TestPruneRejectsUnsupportedSelection(t *testing.T) {
	d := New(Config{Runtime: fake.NewRuntime(), StateDir: t.TempDir()})
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/prune", bytes.NewBufferString(`{"force":true,"service":"app"}`)))
	if w.Code != 400 {
		t.Fatalf("prune silently ignored service selection: %d %s", w.Code, w.Body)
	}
}

func TestFailedStateRemovalKeepsAuthoritativeDesiredState(t *testing.T) {
	rt := fake.NewRuntime()
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
	putSerial(t, d, serialDesired("app", "v1"))
	// Make rollback-state cleanup fail without relying on Unix user permissions.
	path := filepath.Join(d.store.Dir(), "app.production.last-good.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "blocked"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/remove", bytes.NewBufferString(`{"force":true,"service":"app"}`)))
	if w.Code != 500 {
		t.Fatalf("expected cleanup error: %d %s", w.Code, w.Body)
	}
	if _, err := d.store.LoadDesired("app", "production"); err != nil {
		t.Fatalf("failed removal lost authoritative state while retaining its healing target: %v", err)
	}
}

func TestMaintenancePreservesSharedProxyAndActiveDesiredContainers(t *testing.T) {
	for _, operation := range []string{"remove", "prune"} {
		t.Run(operation, func(t *testing.T) {
			rt := fake.NewRuntime()
			d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
			desired := serialDesired("app", "v1")
			putSerial(t, d, desired)
			containers, _ := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": "app"}})
			if err := rt.StopContainer(context.Background(), containers[0].ID, time.Second); err != nil {
				t.Fatal(err)
			}
			proxyID, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{Name: "serve-proxy", Image: "proxy", Labels: map[string]string{"serve.managed": "true", "serve.container_type": "proxy"}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			d.handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/"+operation, bytes.NewBufferString(`{"force":true}`)))
			if w.Code != 200 {
				t.Fatalf("%s: %d %s", operation, w.Code, w.Body)
			}
			if _, err := rt.InspectContainer(context.Background(), proxyID); err != nil {
				t.Fatal("maintenance deleted the shared proxy")
			}
			if operation == "prune" {
				if _, err := rt.InspectContainer(context.Background(), containers[0].ID); err != nil {
					t.Fatal("prune deleted an active desired container awaiting healing")
				}
			}
		})
	}
}

func TestRemoveWaitsForCutoverBeforeSelectingWorkloads(t *testing.T) {
	rt := fake.NewRuntime()
	gate := &reconcileGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	gate.enabled.Store(true)
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: gate})
	done := make(chan struct{})
	go func() { defer close(done); putSerial(t, d, serialDesired("a-slow", "v1")) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("cutover did not start")
	}
	removed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/remove", bytes.NewBufferString(`{"force":true}`)))
		removed <- w
	}()
	select {
	case <-removed:
		close(gate.release)
		<-done
		t.Fatal("remove bypassed in-progress cutover")
	case <-time.After(30 * time.Millisecond):
	}
	close(gate.release)
	<-done
	select {
	case w := <-removed:
		if w.Code != 200 {
			t.Fatalf("remove: %d %s", w.Code, w.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("remove stayed blocked")
	}
	if err := d.reconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	containers, _ := rt.ListContainers(context.Background(), runtime.ContainerFilters{})
	if len(containers) != 0 {
		t.Fatalf("workloads remain after serialized remove: %#v", containers)
	}
}
