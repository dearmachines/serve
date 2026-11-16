package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fakeproxy "github.com/uptimenine/serve/internal/agent/proxy/fake"
	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

// firstSleepBlocks parks the first heal backoff until released, modelling a
// crash-looping service waiting out its backoff.
type firstSleepBlocks struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *firstSleepBlocks) Sleep(ctx context.Context, d time.Duration) error {
	first := false
	s.once.Do(func() { first = true })
	if !first {
		return nil
	}
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return ctx.Err()
}

func dieRunning(t *testing.T, rt *fake.Runtime, service string) runtime.RuntimeEvent {
	t.Helper()
	states, err := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": service}})
	if err != nil || len(states) != 1 {
		t.Fatalf("find %s container: %v %#v", service, err, states)
	}
	rt.Die(states[0].ID, 1, false)
	return runtime.RuntimeEvent{Type: runtime.EventDie, ContainerID: states[0].ID, Name: states[0].Name, Labels: states[0].Labels, ExitCode: 1}
}

func TestCrashLoopBackoffDoesNotDelayHealingOfOtherServices(t *testing.T) {
	rt := fake.NewRuntime()
	sleeper := &firstSleepBlocks{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(sleeper.release)
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}, HealSleeper: sleeper, ErrorLog: io.Discard})
	looping := serialDesired("a-loop", "v1")
	looping.Containers[0].Restart.InitialBackoff = "1s"
	putSerial(t, d, looping)
	putSerial(t, d, serialDesired("b-ok", "v1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.background.Wait() }()

	d.dispatchEvent(ctx, dieRunning(t, rt, "a-loop"))
	select {
	case <-sleeper.entered:
	case <-time.After(time.Second):
		t.Fatal("a-loop heal did not reach backoff")
	}
	dispatched := make(chan struct{})
	go func() { d.dispatchEvent(ctx, dieRunning(t, rt, "b-ok")); close(dispatched) }()
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("event dispatch blocked behind another service's backoff")
	}

	deadline := time.After(2 * time.Second)
	for {
		states, _ := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": "b-ok"}})
		if len(states) == 1 && states[0].Running {
			return
		}
		select {
		case <-deadline:
			t.Fatal("b-ok was not healed while a-loop waited out its backoff")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestStateKeysDoNotCollideForDottedIdentities(t *testing.T) {
	if stateKey("shop.api", "prod") == stateKey("shop", "api.prod") {
		t.Fatal("distinct (service, destination) identities share a lock and healing key")
	}
}

func TestReconcileContinuesPastCorruptDesiredState(t *testing.T) {
	rt := fake.NewRuntime()
	d := New(Config{Runtime: rt, StateDir: t.TempDir(), ProxyManager: fakeproxy.NewManager(), HealthChecker: &reconcileGate{}, ErrorLog: io.Discard})
	putSerial(t, d, serialDesired("good", "v1"))
	corrupt := d.store.Path("broken", "production", "desired")
	if err := os.MkdirAll(filepath.Dir(corrupt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	states, _ := rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": "good"}})
	if err := rt.RemoveContainer(context.Background(), states[0].ID); err != nil {
		t.Fatal(err)
	}

	err := d.reconcileAll(context.Background())

	if err == nil {
		t.Fatal("corrupt desired state was not reported")
	}
	states, _ = rt.ListContainers(context.Background(), runtime.ContainerFilters{Labels: map[string]string{"serve.service": "good"}})
	if len(states) != 1 || !states[0].Running {
		t.Fatal("one corrupt desired-state file stopped reconcile for every service")
	}
}
