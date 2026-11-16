package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

// stuckExecRuntime models a workload whose exec stream never ends and a
// runtime that does not honour cancellation (e.g. a background child keeping
// the attach stream open).
type stuckExecRuntime struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
}

func (r *stuckExecRuntime) ExecContainer(context.Context, runtime.ContainerID, []string) (string, error) {
	r.entered <- struct{}{}
	<-r.release
	return "", nil
}

func TestStuckExecDoesNotBlockReconcileOrRemove(t *testing.T) {
	rt := &stuckExecRuntime{Runtime: fake.NewRuntime(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(rt.release)
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}, ErrorLog: io.Discard})
	putSerial(t, d, serialDesired("app", "v1"))

	body, _ := json.Marshal(ExecRequest{Container: "app-web-v1", Command: []string{"sh", "-c", "sleep 300 &"}})
	go d.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/exec", bytes.NewReader(body)))
	select {
	case <-rt.entered:
	case <-time.After(time.Second):
		t.Fatal("exec did not start")
	}

	reconciled := make(chan error, 1)
	go func() { reconciled <- d.reconcileAll(context.Background()) }()
	select {
	case err := <-reconciled:
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconcile blocked behind a running exec")
	}

	removed := make(chan int, 1)
	go func() {
		data, _ := json.Marshal(map[string]any{"force": true, "service": "app", "destination": "production"})
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/remove", bytes.NewReader(data)))
		removed <- w.Code
	}()
	select {
	case code := <-removed:
		if code != 200 {
			t.Fatalf("remove: %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("remove blocked behind a running exec")
	}
}
