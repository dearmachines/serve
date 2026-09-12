package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

type unavailableListRuntime struct{ runtime.Runtime }

func (unavailableListRuntime) ListContainers(context.Context, runtime.ContainerFilters) ([]runtime.ContainerState, error) {
	return nil, errors.New("Docker connection refused")
}

func TestContainerSelectionReportsDockerFailuresAsServerErrors(t *testing.T) {
	d := New(Config{Runtime: unavailableListRuntime{fake.NewRuntime()}, StateDir: t.TempDir()})
	logs := httptest.NewRecorder()
	d.handler().ServeHTTP(logs, httptest.NewRequest("GET", "/v1/logs?container=app", nil))
	exec := postOperation(t, d, "/v1/exec", ExecRequest{Container: "app", Command: []string{"true"}})
	for _, response := range []*httptest.ResponseRecorder{logs, exec} {
		if response.Code != 500 || !strings.Contains(response.Body.String(), "Docker connection refused") {
			t.Fatalf("Docker failure misclassified: %d %s", response.Code, response.Body)
		}
	}
}
func TestContainerSelectionDistinguishesMissingFromAmbiguous(t *testing.T) {
	rt := fake.NewRuntime()
	d := New(Config{Runtime: rt, StateDir: t.TempDir()})
	for _, name := range []string{"a", "b"} {
		if _, err := rt.CreateContainer(context.Background(), runtime.ContainerSpec{Name: name, Image: "app", Labels: map[string]string{"serve.managed": "true"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		query string
		code  int
	}{{"", 400}, {"?container=missing", 404}} {
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/logs"+test.query, nil))
		if w.Code != test.code {
			t.Fatalf("logs selection %q: %d %s", test.query, w.Code, w.Body)
		}
	}
	w := postOperation(t, d, "/v1/exec", ExecRequest{Container: "missing", Command: []string{"true"}})
	if w.Code != 404 {
		t.Fatalf("missing exec container: %d %s", w.Code, w.Body)
	}
}

func TestMaintenanceReportsCompletedDeletionWhenActualStateWriteFails(t *testing.T) {
	for _, operation := range []string{"remove", "prune"} {
		t.Run(operation, func(t *testing.T) {
			rt := fake.NewRuntime()
			d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}})
			if operation == "remove" {
				putSerial(t, d, removalDesired())
			} else {
				putSerial(t, d, serialDesired("app", "v1"))
				putSerial(t, d, serialDesired("app", "v2"))
			}
			restore := blockStateWrite(t, filepath.Join(d.store.Dir(), "app.production.actual.json"))
			run := func() *httptest.ResponseRecorder {
				if operation == "remove" {
					return removeWeb(t, d)
				}
				return postOperation(t, d, "/v1/prune", PruneRequest{Force: true})
			}
			w := run()
			restore()
			verb := "Removed"
			if operation == "prune" {
				verb = "Pruned"
			}
			if w.Code != 500 || !strings.Contains(w.Body.String(), verb+" 1 container(s)") || !strings.Contains(w.Body.String(), "failed to refresh actual state") {
				t.Fatalf("partial success not explained: %d %s", w.Code, w.Body)
			}
			w = run()
			if w.Code != 200 {
				t.Fatalf("retry: %d %s", w.Code, w.Body)
			}
			actual, err := d.store.LoadActual("app", "production")
			if err != nil {
				t.Fatal(err)
			}
			if len(actual.Containers) != 1 {
				t.Fatalf("retry did not refresh actual state: %#v", actual)
			}
		})
	}
}
