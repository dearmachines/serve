package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/uptimenine/serve/internal/agent/proxy"
	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func removalDesired() planner.DesiredState {
	desired := serialDesired("app", "v1")
	worker := desired.Containers[0]
	worker.Name, worker.Role, worker.Proxy = "app-worker-v1", "worker", false
	worker.Labels = map[string]string{}
	for key, value := range desired.Containers[0].Labels {
		worker.Labels[key] = value
	}
	worker.Labels["serve.role"] = "worker"
	desired.Containers = append(desired.Containers, worker)
	return desired
}
func postOperation(t *testing.T, d *Daemon, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewReader(data)))
	return w
}
func removeWeb(t *testing.T, d *Daemon) *httptest.ResponseRecorder {
	return postOperation(t, d, "/v1/remove", RemoveRequest{Force: true, Service: "app", Destination: "production", Role: "web"})
}

// A filesystem fault that works even when tests run as root. Restore the old
// file afterwards to model an atomic write failure leaving its previous value.
func blockStateWrite(t *testing.T, path string) func() {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

type removalFaultProxy struct {
	proxy.Manager
	fault func()
}

func (p *removalFaultProxy) SetTargets(ctx context.Context, service, role string, targets []proxy.Target, opts proxy.RouteOptions) error {
	if len(targets) == 0 && p.fault != nil {
		fault := p.fault
		p.fault = nil
		fault()
	}
	return p.Manager.SetTargets(ctx, service, role, targets, opts)
}

func TestPartialRemovalRepairsWriteFailuresOnRetry(t *testing.T) {
	for _, kind := range []string{"last-good", "desired"} {
		t.Run(kind, func(t *testing.T) {
			rt := fake.NewRuntime()
			manager := &removalFaultProxy{Manager: fakeproxy.NewManager()}
			d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: manager, HealthChecker: &reconcileGate{}})
			putSerial(t, d, removalDesired())
			var restore func()
			path := filepath.Join(d.store.Dir(), "app.production."+kind+".json")
			if kind == "last-good" {
				restore = blockStateWrite(t, path)
			} else {
				// Let removal enumerate the existing desired state before failing its write.
				manager.fault = func() { restore = blockStateWrite(t, path) }
			}
			w := removeWeb(t, d)
			if w.Code != 500 {
				t.Fatalf("expected write failure: %d %s", w.Code, w.Body)
			}
			if restore == nil {
				t.Fatal("state write was not reached")
			}
			restore()
			desired, err := d.store.LoadDesired("app", "production")
			if err != nil {
				t.Fatal(err)
			}
			if len(desired.Containers) != 2 {
				t.Fatalf("failed baseline update published removal: %#v", desired.Containers)
			}
			if kind == "desired" {
				baseline, err := d.store.LoadLastGood("app", "production")
				if err != nil {
					t.Fatal(err)
				}
				if len(baseline.Containers) != 1 {
					t.Fatalf("baseline was not prepared before desired: %#v", baseline.Containers)
				}
			}
			assertRemovalSurvivesRetryAndRollback(t, d, rt)
		})
	}
}
func TestPartialRemovalRepairsAlreadyStaleRollbackBaseline(t *testing.T) {
	rt := fake.NewRuntime()
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
	desired := removalDesired()
	putSerial(t, d, desired)
	desired.Containers = desired.Containers[1:]
	if err := d.store.SaveDesired(desired); err != nil {
		t.Fatal(err)
	}
	d.setDesired(desired)
	assertRemovalSurvivesRetryAndRollback(t, d, rt)
}
func assertRemovalSurvivesRetryAndRollback(t *testing.T, d *Daemon, rt *fake.Runtime) {
	t.Helper()
	w := removeWeb(t, d)
	if w.Code != 200 {
		t.Fatalf("retry remove: %d %s", w.Code, w.Body)
	}
	lastGood, err := d.store.LoadLastGood("app", "production")
	if err != nil {
		t.Fatal(err)
	}
	if len(lastGood.Containers) != 1 || lastGood.Containers[0].Role != "worker" {
		t.Fatalf("retry left a stale rollback baseline: %#v", lastGood)
	}
	restarted := New(Config{Runtime: rt, StateDir: d.store.Dir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
	if err := restarted.reconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	w = postOperation(t, restarted, "/v1/rollback", RollbackRequest{Service: "app", Destination: "production"})
	if w.Code != 200 {
		t.Fatalf("rollback: %d %s", w.Code, w.Body)
	}
	containers, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].Labels["serve.role"] != "worker" {
		t.Fatalf("rollback resurrected removed role: %#v", containers)
	}
}
