package kamalproxy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/proxy/kamalproxy"
	"github.com/uptimenine/serve/internal/runtime"
	fakeruntime "github.com/uptimenine/serve/internal/runtime/fake"
)

const hostConflict = "Error: host settings conflict with another service\n"

func proxyVerbs(rt *fakeruntime.Runtime) []string {
	var verbs []string
	for _, op := range rt.Operations() {
		if fields := strings.Fields(strings.TrimPrefix(op, "exec_container:serve-proxy:")); len(fields) >= 3 && fields[0] == "kamal-proxy" {
			verbs = append(verbs, fields[1]+" "+fields[2])
		}
	}
	return verbs
}

// kamal-proxy reports a host conflict only after the targets pass their
// health check, so they are known healthy when the legacy route is removed.
// If the replacement then fails, the legacy name must route them again
// rather than leaving the hostnames unrouted.
func TestFailedRouteMigrationRestoresTheLegacyRoute(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	startProxyContainer(t, rt)
	rt.QueueExecResult("serve-proxy", hostConflict, &runtime.ExecExitError{Code: 1, Output: hostConflict})
	rt.QueueExecResult("serve-proxy", "", nil) // remove legacy
	rt.QueueExecResult("serve-proxy", "", errors.New("docker exec: connection reset"))
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})
	rt.ClearOperations()

	err := manager.SetTargets(context.Background(), "my-app", "production", "web", []proxy.Target{webTarget()}, proxy.RouteOptions{Hosts: []string{"a.test"}})

	if err != nil {
		t.Fatalf("the restored legacy route serves the requested targets, so SetTargets must succeed: %v", err)
	}
	want := "deploy my-app/production/web, remove my-app-web, deploy my-app/production/web, deploy my-app-web"
	if got := strings.Join(proxyVerbs(rt), ", "); got != want {
		t.Fatalf("proxy calls = %s, want %s", got, want)
	}
	restore := lastExec(t, rt, "kamal-proxy deploy my-app-web")
	for _, arg := range []string{"--target=my-app-web-production-abc123-r1:3000", "--host=a.test", "--health-check-path=/up"} {
		if !strings.Contains(restore, arg) {
			t.Fatalf("restored legacy route %q is missing %s", restore, arg)
		}
	}

	// The migration is retried on the next apply instead of being cached.
	rt.ClearOperations()
	if err := manager.SetTargets(context.Background(), "my-app", "production", "web", []proxy.Target{webTarget()}, proxy.RouteOptions{Hosts: []string{"a.test"}}); err != nil {
		t.Fatal(err)
	}
	if got := proxyVerbs(rt); len(got) == 0 || got[0] != "deploy my-app/production/web" {
		t.Fatalf("migration was not retried after restoring the legacy route: %v", got)
	}
}

func TestFailedRouteMigrationWithUnrestorableLegacyRouteIsReported(t *testing.T) {
	rt := fakeruntime.NewRuntime()
	startProxyContainer(t, rt)
	rt.QueueExecResult("serve-proxy", hostConflict, &runtime.ExecExitError{Code: 1, Output: hostConflict})
	rt.QueueExecResult("serve-proxy", "", nil)
	rt.QueueExecResult("serve-proxy", "", errors.New("replacement failed"))
	rt.QueueExecResult("serve-proxy", "", errors.New("restore failed"))
	manager := kamalproxy.New(rt, kamalproxy.Options{Network: "serve"})

	err := manager.SetTargets(context.Background(), "my-app", "production", "web", []proxy.Target{webTarget()}, proxy.RouteOptions{Hosts: []string{"a.test"}})

	if err == nil || !strings.Contains(err.Error(), "replacement failed") || !strings.Contains(err.Error(), "restore failed") {
		t.Fatalf("an unrouted hostname must be reported with both failures, got %v", err)
	}
}

// cancellingRuntime cancels the request once the legacy route is removed and
// fails execs on a done context, as the Docker runtime does.
type cancellingRuntime struct {
	*fakeruntime.Runtime
	cancel context.CancelFunc
}

func (r *cancellingRuntime) ExecContainer(ctx context.Context, id runtime.ContainerID, cmd []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	output, err := r.Runtime.ExecContainer(ctx, id, cmd)
	if len(cmd) >= 2 && cmd[1] == "remove" {
		r.cancel()
	}
	return output, err
}

func TestRouteMigrationCancelledAfterRemovalStillRestoresTheLegacyRoute(t *testing.T) {
	fake := fakeruntime.NewRuntime()
	startProxyContainer(t, fake)
	fake.QueueExecResult("serve-proxy", hostConflict, &runtime.ExecExitError{Code: 1, Output: hostConflict})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := kamalproxy.New(&cancellingRuntime{Runtime: fake, cancel: cancel}, kamalproxy.Options{Network: "serve"})
	fake.ClearOperations()

	_ = manager.SetTargets(ctx, "my-app", "production", "web", []proxy.Target{webTarget()}, proxy.RouteOptions{Hosts: []string{"a.test"}})

	if got := proxyVerbs(fake); len(got) == 0 || got[len(got)-1] != "deploy my-app-web" {
		t.Fatalf("a cancelled request left the hostname unrouted; proxy calls: %v", got)
	}
}
