package healing_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/healing"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

type noopSleeper struct{}

func (noopSleeper) Sleep(context.Context, time.Duration) error { return nil }

// The daemon heals each service on its own goroutine, all sharing one
// supervisor. Restart bookkeeping for distinct services must not race.
// Run with -race.
func TestConcurrentHealingOfDistinctServicesDoesNotRace(t *testing.T) {
	rt := fake.NewRuntime()
	supervisor := healing.NewSupervisor(rt, healing.WithSleeper(noopSleeper{}))

	var wg sync.WaitGroup
	for _, service := range []string{"alpha", "beta", "gamma", "delta"} {
		desired := desiredState("v1")
		desired.Service = service
		desired.Containers[0].Name = service + "-web-production-v1-r1"
		desired.Containers[0].Proxy = false
		desired.Containers[0].Labels["serve.service"] = service
		event := runtime.RuntimeEvent{Type: runtime.EventDie, Name: desired.Containers[0].Name, ExitCode: 1}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, err := supervisor.HandleRuntimeEvent(context.Background(), event, desired); err != nil {
					t.Errorf("heal %s: %v", service, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
