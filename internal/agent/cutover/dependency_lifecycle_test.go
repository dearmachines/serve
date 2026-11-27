package cutover_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/cutover"
	"github.com/uptimenine/serve/internal/agent/health"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
	fakeruntime "github.com/uptimenine/serve/internal/runtime/fake"
)

// dependency builds a dependency the way the planner does: named by its spec
// and without an application version label.
func dependency(role string, spec string) planner.Container {
	return planner.Container{
		Name:          fmt.Sprintf("my-app-%s-production-%s-r1", role, spec),
		Role:          role,
		ContainerType: "accessory",
		Image:         "postgres:" + spec,
		Replica:       1,
		Labels: map[string]string{
			"serve.managed":        "true",
			"serve.service":        "my-app",
			"serve.destination":    "production",
			"serve.role":           role,
			"serve.replica":        "1",
			"serve.container_type": "accessory",
			"serve.spec_hash":      spec,
		},
	}
}

func statefulDependency(role string, spec string) planner.Container {
	container := dependency(role, spec)
	container.Volumes = []string{role + "-data:/data"}
	return container
}

func desiredWith(version string, dependencies ...planner.Container) planner.DesiredState {
	desired := desiredState(version, 1, false)
	desired.Containers = append(desired.Containers, dependencies...)
	return desired
}

func (e *env) apply(t *testing.T, desired planner.DesiredState) {
	t.Helper()
	e.checker.SetStatus(desired.Containers[0].Name, health.Healthy)
	if err := e.engine.Apply(context.Background(), desired); err != nil {
		t.Fatalf("Apply %s: %v", desired.Version, err)
	}
}

func (e *env) containerNamed(t *testing.T, name string) (runtime.ContainerState, bool) {
	t.Helper()
	states, err := e.rt.ListContainers(context.Background(), runtime.ContainerFilters{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	for _, state := range states {
		if state.Name == name {
			return state, true
		}
	}
	return runtime.ContainerState{}, false
}

func (e *env) mustContainer(t *testing.T, name string) runtime.ContainerState {
	t.Helper()
	state, ok := e.containerNamed(t, name)
	if !ok {
		t.Fatalf("container %s not found", name)
	}
	return state
}

func TestUnchangedDependencyKeepsRunningAcrossApplicationDeploys(t *testing.T) {
	env := newEnv(t)
	database := statefulDependency("postgres", "16")
	database.Aliases = []string{"database"}
	database.Ports = []planner.Port{{Name: "tcp", ContainerPort: 5432, HostPort: 15432, HostIP: "127.0.0.1"}}
	env.apply(t, desiredWith("abc123", database))
	before := env.mustContainer(t, database.Name)
	env.rt.ClearOperations()

	env.apply(t, desiredWith("def456", database))

	after := env.mustContainer(t, database.Name)
	if after.ID != before.ID || !after.Running {
		t.Fatalf("unchanged dependency was replaced or stopped: before %+v, after %+v", before, after)
	}
	if !reflect.DeepEqual(after.Aliases, []string{"database"}) {
		t.Fatalf("unchanged dependency aliases = %#v", after.Aliases)
	}
	for _, operation := range env.rt.Operations() {
		if strings.HasSuffix(operation, ":"+string(before.ID)) || operation == "create_container:"+database.Name {
			t.Fatalf("unchanged dependency was touched during deploy: %v", env.rt.Operations())
		}
	}
	if old := env.containersByVersion(t, "abc123"); len(old) != 1 || old[0].Running {
		t.Fatalf("old application version must still be retired, got %+v", old)
	}
}

func TestChangedStatefulDependencyStopsPreviousBeforeStartingReplacement(t *testing.T) {
	env := newEnv(t)
	previous := statefulDependency("postgres", "16")
	env.apply(t, desiredWith("abc123", previous))
	previousState := env.mustContainer(t, previous.Name)
	env.rt.ClearOperations()

	replacement := statefulDependency("postgres", "17")
	env.apply(t, desiredWith("def456", replacement))

	operations := env.rt.Operations()
	stopPrevious := operationIndex(operations, "stop_container:"+string(previousState.ID))
	createReplacement := operationIndex(operations, "create_container:"+replacement.Name)
	if stopPrevious < 0 || createReplacement < 0 || stopPrevious > createReplacement {
		t.Fatalf("previous stateful dependency must stop before its replacement starts: %v", operations)
	}
	if state := env.mustContainer(t, replacement.Name); !state.Running {
		t.Fatalf("replacement dependency is not running: %+v", state)
	}
	if state := env.mustContainer(t, previous.Name); state.Running {
		t.Fatalf("previous dependency is still running: %+v", state)
	}
}

func TestChangedStatefulDependencyStartupFailureRestartsPrevious(t *testing.T) {
	env := newEnv(t)
	previous := statefulDependency("postgres", "16")
	env.apply(t, desiredWith("abc123", previous))
	previousState := env.mustContainer(t, previous.Name)
	env.rt.ClearOperations()

	starter := &failingStarter{delegate: reconciler.New(env.rt), failAt: 2}
	engine := cutover.New(cutover.Deps{
		Runtime: env.rt, Starter: starter, Health: env.checker,
		Proxy: env.proxy, LastGood: env.store, Sleeper: noopSleeper{},
	})
	candidate := desiredWith("def456", statefulDependency("postgres", "17"))
	env.checker.SetStatus(candidate.Containers[0].Name, health.Healthy)

	err := engine.Apply(context.Background(), candidate)

	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("Apply error = %v, want dependency startup failure", err)
	}
	operations := env.rt.Operations()
	stopPrevious := operationIndex(operations, "stop_container:"+string(previousState.ID))
	restartPrevious := operationIndexAfter(operations, "start_container:"+string(previousState.ID), stopPrevious+1)
	if stopPrevious < 0 || restartPrevious < 0 {
		t.Fatalf("previous dependency was not stopped and restarted: %v", operations)
	}
	if state := env.mustContainer(t, previous.Name); state.ID != previousState.ID || !state.Running {
		t.Fatalf("previous dependency is not running after the failed deploy: %+v", state)
	}
	if old := env.containersByVersion(t, "abc123"); len(old) != 1 || !old[0].Running {
		t.Fatalf("old application version is not serving after the failed deploy: %+v", old)
	}
}

func TestChangedStatelessDependencyHandsOffAliasWithoutStoppingFirst(t *testing.T) {
	env := newEnv(t)
	previous := dependency("cache", "7")
	previous.Aliases = []string{"cache"}
	env.apply(t, desiredWith("abc123", previous))
	previousState := env.mustContainer(t, previous.Name)
	env.rt.ClearOperations()

	replacement := dependency("cache", "8")
	replacement.Aliases = []string{"cache"}
	env.apply(t, desiredWith("def456", replacement))

	operations := env.rt.Operations()
	createReplacement := operationIndex(operations, "create_container:"+replacement.Name)
	stopPrevious := operationIndex(operations, "stop_container:"+string(previousState.ID))
	if createReplacement < 0 || stopPrevious < 0 || createReplacement > stopPrevious {
		t.Fatalf("stateless dependency must start its replacement before the previous one stops: %v", operations)
	}
	if state := env.mustContainer(t, replacement.Name); !state.Running || !reflect.DeepEqual(state.Aliases, []string{"cache"}) {
		t.Fatalf("replacement dependency = %+v", state)
	}
	if state := env.mustContainer(t, previous.Name); state.Running || len(state.Aliases) != 0 {
		t.Fatalf("previous dependency kept running or kept its aliases: %+v", state)
	}
}

func TestDependencyRetentionCountsSpecsNotApplicationVersions(t *testing.T) {
	env := newEnv(t)
	cache := dependency("cache", "7")

	deploys := []struct {
		version  string
		database string
	}{
		{"v1", "14"}, {"v2", "15"}, {"v3", "15"}, {"v4", "16"}, {"v5", "17"}, {"v6", "17"},
	}
	for _, deploy := range deploys {
		desired := desiredWith(deploy.version, statefulDependency("postgres", deploy.database), cache)
		desired.RetainContainers = 2
		env.apply(t, desired)
	}

	if state := env.mustContainer(t, cache.Name); !state.Running {
		t.Fatalf("unchanged dependency must survive retention: %+v", state)
	}
	if state := env.mustContainer(t, statefulDependency("postgres", "17").Name); !state.Running {
		t.Fatalf("current dependency spec must run: %+v", state)
	}
	if state := env.mustContainer(t, statefulDependency("postgres", "16").Name); state.Running {
		t.Fatalf("previous dependency spec must be retained stopped: %+v", state)
	}
	for _, spec := range []string{"14", "15"} {
		if state, found := env.containerNamed(t, statefulDependency("postgres", spec).Name); found {
			t.Fatalf("dependency spec %s must be pruned per retain_containers, got %+v", spec, state)
		}
	}
}

func TestVersionedDependencyFromEarlierReleaseIsReplacedOnce(t *testing.T) {
	env := newEnv(t)
	legacy := statefulDependency("postgres", "abc123")
	legacy.Image = "postgres:16"
	legacy.Labels["serve.version"] = "abc123"
	env.apply(t, desiredWith("abc123", legacy))
	legacyState := env.mustContainer(t, legacy.Name)
	env.rt.ClearOperations()

	current := statefulDependency("postgres", "16")
	env.apply(t, desiredWith("def456", current))

	operations := env.rt.Operations()
	stopLegacy := operationIndex(operations, "stop_container:"+string(legacyState.ID))
	createCurrent := operationIndex(operations, "create_container:"+current.Name)
	if stopLegacy < 0 || createCurrent < 0 || stopLegacy > createCurrent {
		t.Fatalf("versioned dependency must stop before its replacement starts: %v", operations)
	}
	currentState := env.mustContainer(t, current.Name)
	env.rt.ClearOperations()

	env.apply(t, desiredWith("ghi789", current))

	if state := env.mustContainer(t, current.Name); state.ID != currentState.ID || !state.Running {
		t.Fatalf("dependency was replaced again on the following deploy: %+v", state)
	}
}

func TestReapplyingPreviousStateRestoresPreviousDependencySpec(t *testing.T) {
	env := newEnv(t)
	previous := desiredWith("abc123", statefulDependency("postgres", "16"))
	env.apply(t, previous)
	env.apply(t, desiredWith("def456", statefulDependency("postgres", "17")))

	env.apply(t, previous)

	if state := env.mustContainer(t, statefulDependency("postgres", "16").Name); !state.Running {
		t.Fatalf("previous dependency spec is not running after rollback: %+v", state)
	}
	if state := env.mustContainer(t, statefulDependency("postgres", "17").Name); state.Running {
		t.Fatalf("newer dependency spec is still running after rollback: %+v", state)
	}
}

func dockerRestartedDependency(role string, spec string) planner.Container {
	container := statefulDependency(role, spec)
	container.Restart = planner.Restart{Controller: "docker", Policy: "always"}
	return container
}

// Docker's "always" policy restarts a stopped container when the daemon
// restarts, which would bring a retained dependency back next to its
// replacement on the same volume.
func TestRetainedDependencyIsNotRestartedByDocker(t *testing.T) {
	env := newEnv(t)
	previous := desiredWith("abc123", dockerRestartedDependency("postgres", "16"))
	env.apply(t, previous)
	replacement := dockerRestartedDependency("postgres", "17")
	env.apply(t, desiredWith("def456", replacement))

	retained := env.mustContainer(t, dockerRestartedDependency("postgres", "16").Name)
	if retained.Running || retained.Restart.Policy != "no" {
		t.Fatalf("retained dependency can be restarted by Docker: running=%v restart=%+v", retained.Running, retained.Restart)
	}
	if active := env.mustContainer(t, replacement.Name); active.Restart.Policy != "always" {
		t.Fatalf("active dependency restart policy = %+v, want always", active.Restart)
	}

	env.apply(t, previous)

	reactivated := env.mustContainer(t, dockerRestartedDependency("postgres", "16").Name)
	if !reactivated.Running || reactivated.Restart.Policy != "always" {
		t.Fatalf("reactivated dependency lost its restart policy: running=%v restart=%+v", reactivated.Running, reactivated.Restart)
	}
	if retired := env.mustContainer(t, replacement.Name); retired.Restart.Policy != "no" {
		t.Fatalf("retired dependency restart policy = %+v, want no", retired.Restart)
	}
}

// cancelAwareRuntime refuses work on a done context, as the Docker client does.
type cancelAwareRuntime struct {
	*fakeruntime.Runtime
}

func (r *cancelAwareRuntime) ListContainers(ctx context.Context, filters runtime.ContainerFilters) ([]runtime.ContainerState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.Runtime.ListContainers(ctx, filters)
}

func (r *cancelAwareRuntime) StartContainer(ctx context.Context, id runtime.ContainerID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Runtime.StartContainer(ctx, id)
}

func (r *cancelAwareRuntime) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Runtime.StopContainer(ctx, id, timeout)
}

func (r *cancelAwareRuntime) RemoveContainer(ctx context.Context, id runtime.ContainerID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Runtime.RemoveContainer(ctx, id)
}

// cancelingStarter simulates the deploy client disconnecting while the
// engine starts its cancelAt-th container.
type cancelingStarter struct {
	delegate *reconciler.Reconciler
	cancel   context.CancelFunc
	calls    int
	cancelAt int
}

func (s *cancelingStarter) EnsureContainer(ctx context.Context, desired planner.DesiredState, container planner.Container) (reconciler.EnsureResult, error) {
	s.calls++
	if s.calls == s.cancelAt {
		s.cancel()
		return reconciler.EnsureResult{}, ctx.Err()
	}
	return s.delegate.EnsureContainer(ctx, desired, container)
}

func TestClientDisconnectAfterStoppingStatefulDependencyRestartsPrevious(t *testing.T) {
	env := newEnv(t)
	previous := statefulDependency("postgres", "16")
	env.apply(t, desiredWith("abc123", previous))
	previousState := env.mustContainer(t, previous.Name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &cancelAwareRuntime{Runtime: env.rt}
	starter := &cancelingStarter{delegate: reconciler.New(rt), cancel: cancel, cancelAt: 2}
	engine := cutover.New(cutover.Deps{
		Runtime: rt, Starter: starter, Health: env.checker,
		Proxy: env.proxy, LastGood: env.store, Sleeper: noopSleeper{},
	})
	candidate := desiredWith("def456", statefulDependency("postgres", "17"))
	env.checker.SetStatus(candidate.Containers[0].Name, health.Healthy)

	err := engine.Apply(ctx, candidate)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context cancellation", err)
	}
	if state := env.mustContainer(t, previous.Name); state.ID != previousState.ID || !state.Running {
		t.Fatalf("previous dependency is not running after the client disconnected: %+v", state)
	}
	if candidates := env.containersByVersion(t, "def456"); len(candidates) != 0 {
		t.Fatalf("candidate containers were left behind after the client disconnected: %+v", candidates)
	}
}
