package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/health"
	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

type reconcileGate struct {
	enabled atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *reconcileGate) Check(ctx context.Context, target health.Target) (health.Status, error) {
	if g.enabled.Load() && target.ContainerName == "a-slow-web-v1" {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return health.Unhealthy, ctx.Err()
		}
	}
	return health.Healthy, nil
}
func serialDesired(service, version string) planner.DesiredState {
	return planner.DesiredState{Service: service, Destination: "production", Host: "localhost", Version: version, Network: "serve", RetainContainers: 5, Containers: []planner.Container{{Name: service + "-web-" + version, Role: "web", ContainerType: "app", Image: "app:" + version, Replica: 1, Proxy: true, Ports: []planner.Port{{ContainerPort: 3000}}, Healthcheck: &planner.Healthcheck{Type: "http", Port: 3000, Path: "/up", Retries: 1}, Labels: map[string]string{"serve.managed": "true", "serve.service": service, "serve.destination": "production", "serve.version": version, "serve.role": "web", "serve.container_type": "app", "serve.replica": "1"}}}}
}
func putSerial(t *testing.T, d *Daemon, desired planner.DesiredState) {
	t.Helper()
	data, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/desired-state", bytes.NewReader(data)))
	if w.Code != 200 {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
}
func TestRemovePersistsIntentAndDoesNotResurrectRoles(t *testing.T) {
	for _, role := range []string{"", "web"} {
		t.Run("role="+role, func(t *testing.T) {
			rt := fake.NewRuntime()
			d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
			desired := serialDesired("app", "v1")
			worker := desired.Containers[0]
			worker.Name = "app-worker-v1"
			worker.Role = "worker"
			worker.Proxy = false
			worker.Labels = map[string]string{}
			for k, v := range desired.Containers[0].Labels {
				worker.Labels[k] = v
			}
			worker.Labels["serve.role"] = "worker"
			desired.Containers = append(desired.Containers, worker)
			putSerial(t, d, desired)
			data, _ := json.Marshal(map[string]any{"force": true, "service": "app", "destination": "production", "role": role})
			w := httptest.NewRecorder()
			d.handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/remove", bytes.NewReader(data)))
			if w.Code != 200 {
				t.Fatalf("remove: %d %s", w.Code, w.Body)
			}
			actual, err := d.store.LoadActual("app", "production")
			if err != nil {
				t.Fatal(err)
			}
			wantActual := 0
			if role != "" {
				wantActual = 1
			}
			if len(actual.Containers) != wantActual {
				t.Fatalf("actual state still includes removed roles: %#v", actual)
			}
			// Reconstruct the agent to verify persisted intent, not just in-memory suppression.
			restarted := New(Config{Runtime: rt, StateDir: d.store.Dir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
			if err := restarted.reconcileAll(context.Background()); err != nil {
				t.Fatal(err)
			}
			containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": "app"}})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if role != "" {
				want = 1
			}
			if len(containers) != want {
				t.Fatalf("removed workloads resurrected: %#v", containers)
			}
			if want == 1 && containers[0].Labels["serve.role"] != "worker" {
				t.Fatalf("wrong remaining role: %#v", containers)
			}
		})
	}
}

func TestReconcileReloadsDesiredAfterWaitingBehindDeployment(t *testing.T) {
	rt := fake.NewRuntime()
	gate := &reconcileGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: gate, ErrorLog: io.Discard})
	putSerial(t, d, serialDesired("a-slow", "v1"))
	putSerial(t, d, serialDesired("z-target", "v1"))
	gate.enabled.Store(true)
	done := make(chan error, 1)
	go func() { done <- d.reconcileAll(context.Background()) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("reconcile did not start")
	}
	putSerial(t, d, serialDesired("z-target", "v2"))
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": "z-target"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range containers {
		if c.Running && c.Labels["serve.version"] != "v2" {
			t.Fatalf("stale reconcile restored %s after successful deployment", c.Name)
		}
	}
	desired, ok := d.getDesired(stateKey("z-target", "production"))
	if !ok || desired.Version != "v2" {
		t.Fatalf("healing target = %s, want v2", desired.Version)
	}
}
