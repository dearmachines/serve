//go:build integration

package kamalproxy_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/proxy/kamalproxy"
	"github.com/uptimenine/serve/internal/runtime"
	dockerruntime "github.com/uptimenine/serve/internal/runtime/docker"
)

func TestPinnedProxyRemovalAfterRestartAndWhenAlreadyAbsent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rt, err := dockerruntime.NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	network := fmt.Sprintf("serve-proxy-removal-%d", time.Now().UnixNano())
	if err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: network}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.RemoveNetwork(context.Background(), network); err != nil {
			t.Error(err)
		}
	})
	for _, image := range []string{kamalproxy.DefaultImage, "busybox:1.36"} {
		if err := rt.PullImage(ctx, image); err != nil {
			t.Fatal(err)
		}
	}
	start := func(spec runtime.ContainerSpec) runtime.ContainerID {
		t.Helper()
		id, err := rt.CreateContainer(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := rt.RemoveContainer(context.Background(), id); err != nil {
				t.Error(err)
			}
		})
		if err := rt.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Cleanup containers before the network, without touching the host's shared
	// proxy, ports, or persistent volumes.
	proxyName := network + "-proxy"
	id := start(runtime.ContainerSpec{Name: proxyName, Image: kamalproxy.DefaultImage, Network: network, Labels: map[string]string{"serve.managed": "true", "serve.container_type": "proxy"}})
	appName := network + "-app"
	start(runtime.ContainerSpec{Name: appName, Image: "busybox:1.36", Network: network, Command: []string{"sh", "-c", "mkdir -p /tmp/www; echo ok >/tmp/www/index.html; httpd -f -p 8080 -h /tmp/www"}})
	waitReady := func() {
		t.Helper()
		for {
			if _, err := rt.ExecContainer(ctx, id, []string{"kamal-proxy", "list"}); err == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	assertAbsent := func() {
		t.Helper()
		output, err := rt.ExecContainer(ctx, id, []string{"kamal-proxy", "remove", "app/production/web"})
		var exit *runtime.ExecExitError
		if !errors.As(err, &exit) || exit.Code != 1 || strings.TrimSpace(output) != "Error: service not found" {
			t.Fatalf("pinned not-found contract: %q %v", output, err)
		}
	}
	waitReady()
	assertAbsent()
	options := kamalproxy.Options{ContainerName: proxyName, Network: network}
	fresh := kamalproxy.New(rt, options)
	if err := fresh.SetTargets(ctx, "app", "production", "web", nil, proxy.RouteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, restartProxy := range []bool{false, true} {
		manager := kamalproxy.New(rt, options)
		target := proxy.Target{Service: "app", Destination: "production", Role: "web", ContainerName: appName, Port: 8080, HealthPath: "/"}
		if err := manager.SetTargets(ctx, "app", "production", "web", []proxy.Target{target}, proxy.RouteOptions{}); err != nil {
			t.Fatal(err)
		}
		if restartProxy {
			if err := rt.StopContainer(ctx, id, time.Second); err != nil {
				t.Fatal(err)
			}
			if err := rt.StartContainer(ctx, id); err != nil {
				t.Fatal(err)
			}
			waitReady()
		}
		output, err := rt.ExecContainer(ctx, id, []string{"kamal-proxy", "list"})
		if err != nil || !strings.Contains(output, "app/production/web") {
			t.Fatalf("route absent before removal: %q %v", output, err)
		}
		fresh = kamalproxy.New(rt, options)
		for i := 0; i < 2; i++ {
			if err := fresh.SetTargets(ctx, "app", "production", "web", nil, proxy.RouteOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		assertAbsent()
	}
}

// proxyFixture starts a private kamal-proxy and returns helpers bound to it,
// without touching the host's shared proxy, ports, or volumes.
type proxyFixture struct {
	t       *testing.T
	ctx     context.Context
	rt      *dockerruntime.Runtime
	network string
	proxyID runtime.ContainerID
	options kamalproxy.Options
}

func newProxyFixture(t *testing.T, ctx context.Context) *proxyFixture {
	t.Helper()
	rt, err := dockerruntime.NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	f := &proxyFixture{t: t, ctx: ctx, rt: rt, network: fmt.Sprintf("serve-proxy-it-%d", time.Now().UnixNano())}
	if err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: f.network}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.RemoveNetwork(context.Background(), f.network); err != nil {
			t.Error(err)
		}
	})
	for _, image := range []string{kamalproxy.DefaultImage, "busybox:1.36"} {
		if err := rt.PullImage(ctx, image); err != nil {
			t.Fatal(err)
		}
	}
	proxyName := f.network + "-proxy"
	f.proxyID = f.start(runtime.ContainerSpec{Name: proxyName, Image: kamalproxy.DefaultImage, Network: f.network, Labels: map[string]string{"serve.managed": "true", "serve.container_type": "proxy"}})
	f.options = kamalproxy.Options{ContainerName: proxyName, Network: f.network}
	for {
		if _, err := rt.ExecContainer(ctx, f.proxyID, []string{"kamal-proxy", "list"}); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return f
}

func (f *proxyFixture) start(spec runtime.ContainerSpec) runtime.ContainerID {
	f.t.Helper()
	id, err := f.rt.CreateContainer(f.ctx, spec)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = f.rt.RemoveContainer(context.Background(), id) })
	if err := f.rt.StartContainer(f.ctx, id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *proxyFixture) backend(suffix string, body string) (string, runtime.ContainerID) {
	name := f.network + "-" + suffix
	id := f.start(runtime.ContainerSpec{Name: name, Image: "busybox:1.36", Network: f.network, Command: []string{"sh", "-c", "mkdir -p /w; echo " + body + " >/w/index.html; httpd -f -p 8080 -h /w"}})
	return name, id
}

func (f *proxyFixture) kamal(args ...string) (string, error) {
	return f.rt.ExecContainer(f.ctx, f.proxyID, append([]string{"kamal-proxy"}, args...))
}

// get fetches / through the proxy with a Host header, from inside the network.
func (f *proxyFixture) get(host string) string {
	f.t.Helper()
	client := f.start(runtime.ContainerSpec{Name: fmt.Sprintf("%s-curl-%d", f.network, time.Now().UnixNano()), Image: "busybox:1.36", Network: f.network, Command: []string{"sleep", "60"}})
	output, _ := f.rt.ExecContainer(f.ctx, client, []string{"wget", "-qO-", "-T", "3", "--header", "Host: " + host, "http://" + f.options.ContainerName + "/"})
	return strings.TrimSpace(output)
}

func TestHealingLastTargetDoesNotLetHostFallThroughToCatchAll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newProxyFixture(t, ctx)
	appName, _ := f.backend("app", "app")
	otherName, _ := f.backend("other", "other")
	manager := kamalproxy.New(f.rt, f.options)

	other := proxy.Target{Service: "other", Destination: "production", Role: "web", ContainerName: otherName, Port: 8080, HealthPath: "/"}
	if err := manager.SetTargets(ctx, "other", "production", "web", []proxy.Target{other}, proxy.RouteOptions{}); err != nil {
		t.Fatal(err)
	}
	app := proxy.Target{Service: "app", Destination: "production", Role: "web", ContainerName: appName, Port: 8080, HealthPath: "/"}
	if err := manager.SetTargets(ctx, "app", "production", "web", []proxy.Target{app}, proxy.RouteOptions{Hosts: []string{"app.test"}}); err != nil {
		t.Fatal(err)
	}
	if got := f.get("app.test"); got != "app" {
		t.Fatalf("app.test before heal = %q", got)
	}

	if err := manager.RemoveTarget(ctx, app); err != nil {
		t.Fatal(err)
	}

	if got := f.get("app.test"); got == "other" {
		t.Fatal("app.test fell through to another service's catch-all route after healing removed its last target")
	}
}

func TestLegacyRouteIsMigratedToDestinationScopedName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newProxyFixture(t, ctx)
	appName, _ := f.backend("app", "app")
	// An agent from before destination-scoped names left this route behind.
	if _, err := f.kamal("deploy", "app-web", "--target="+appName+":8080", "--host=app.test", "--health-check-path=/"); err != nil {
		t.Fatal(err)
	}
	manager := kamalproxy.New(f.rt, f.options)
	target := proxy.Target{Service: "app", Destination: "production", Role: "web", ContainerName: appName, Port: 8080, HealthPath: "/"}

	if err := manager.SetTargets(ctx, "app", "production", "web", []proxy.Target{target}, proxy.RouteOptions{Hosts: []string{"app.test"}}); err != nil {
		t.Fatalf("migrate legacy route: %v", err)
	}

	list, err := f.kamal("list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "app/production/web") || strings.Contains(list, "app-web ") {
		t.Fatalf("routes after migration:\n%s", list)
	}
	if got := f.get("app.test"); got != "app" {
		t.Fatalf("app.test after migration = %q", got)
	}
	if err := manager.SetTargets(ctx, "app", "production", "web", nil, proxy.RouteOptions{}); err != nil {
		t.Fatalf("remove destination-scoped route: %v", err)
	}
	if list, _ := f.kamal("list"); strings.Contains(list, "app/production/web") {
		t.Fatalf("route still listed after removal:\n%s", list)
	}
}

// kamal-proxy health-checks targets before it reports a host conflict, so an
// unhealthy candidate fails without the legacy route ever being removed. The
// migration's restore path relies on this ordering.
func TestUnhealthyCandidateNeverRemovesTheLegacyRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newProxyFixture(t, ctx)
	oldName, _ := f.backend("old", "old")
	// An agent from before destination-scoped names left this route behind.
	if _, err := f.kamal("deploy", "app-web", "--target="+oldName+":8080", "--host=app.test", "--health-check-path=/"); err != nil {
		t.Fatal(err)
	}
	// The candidate never becomes healthy: nothing listens on its port.
	candidateName := f.network + "-candidate"
	f.start(runtime.ContainerSpec{Name: candidateName, Image: "busybox:1.36", Network: f.network, Command: []string{"sleep", "120"}})
	manager := kamalproxy.New(f.rt, f.options)
	candidate := proxy.Target{Service: "app", Destination: "production", Role: "web", ContainerName: candidateName, Port: 8080, HealthPath: "/"}

	err := manager.SetTargets(ctx, "app", "production", "web", []proxy.Target{candidate}, proxy.RouteOptions{Hosts: []string{"app.test"}, DeployTimeout: 3 * time.Second})

	if err == nil {
		t.Fatal("an unhealthy candidate was routed")
	}
	if got := f.get("app.test"); got != "old" {
		list, _ := f.kamal("list")
		t.Fatalf("app.test after failed migration = %q, want the old app still routed; routes:\n%s", got, list)
	}
}
