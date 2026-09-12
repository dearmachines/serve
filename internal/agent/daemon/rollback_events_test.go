package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/healing"
	"github.com/uptimenine/serve/internal/agent/health"
	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

type rollbackEventSink struct{ events chan healing.LifecycleEvent }

func (s rollbackEventSink) Emit(ctx context.Context, event healing.LifecycleEvent) error {
	s.events <- event
	return nil
}

type rollbackHealthGate struct {
	entered, release chan struct{}
	status           health.Status
}

func (g rollbackHealthGate) Check(ctx context.Context, target health.Target) (health.Status, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return g.status, nil
	case <-ctx.Done():
		return health.Unhealthy, ctx.Err()
	}
}
func TestRollbackPublishesEventsBeforeCompletionAndReturnsAnOutcome(t *testing.T) {
	for _, status := range []health.Status{health.Healthy, health.Unhealthy} {
		t.Run(string(status), func(t *testing.T) {
			sink := rollbackEventSink{make(chan healing.LifecycleEvent, 4)}
			gate := rollbackHealthGate{make(chan struct{}, 1), make(chan struct{}), status}
			d := New(Config{Runtime: fake.NewRuntime(), StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: gate, EventSink: sink})
			if err := d.store.SaveLastGood(serialDesired("app", "v1")); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- postOperation(t, d, "/v1/rollback", RollbackRequest{Service: "app", Destination: "production"})
			}()
			select {
			case <-gate.entered:
			case <-time.After(time.Second):
				t.Fatal("rollback health check never started")
			}
			select {
			case event := <-sink.events:
				if event.Name != "rollback_started" || event.Service != "app" || event.Version != "v1" {
					t.Errorf("start event: %#v", event)
				}
			default:
				t.Error("rollback_started was not published to the agent sink before health checking")
			}
			close(gate.release)
			var w *httptest.ResponseRecorder
			select {
			case w = <-done:
			case <-time.After(time.Second):
				t.Fatal("rollback did not finish")
			}
			var result struct{ Status, Output, Error string }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("missing structured outcome: %d %s", w.Code, w.Body)
			}
			wantStatus, wantEvent := "completed", "rollback_completed"
			if status == health.Unhealthy {
				wantStatus, wantEvent = "failed", "rollback_failed"
				if !strings.Contains(result.Error, "health") {
					t.Fatalf("missing failure reason: %#v", result)
				}
			} else if result.Error != "" {
				t.Fatalf("unexpected failure: %#v", result)
			}
			if result.Status != wantStatus || !strings.Contains(result.Output, "rollback_started") || !strings.Contains(result.Output, wantEvent) {
				t.Fatalf("incomplete outcome: %#v", result)
			}
			select {
			case event := <-sink.events:
				if event.Name != wantEvent {
					t.Fatalf("terminal event: %#v", event)
				}
			default:
				t.Fatal("terminal event was not recorded by agent")
			}
		})
	}
}
