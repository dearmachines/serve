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
		output, err := rt.ExecContainer(ctx, id, []string{"kamal-proxy", "remove", "app-web"})
		var exit *runtime.ExecExitError
		if !errors.As(err, &exit) || exit.Code != 1 || strings.TrimSpace(output) != "Error: service not found" {
			t.Fatalf("pinned not-found contract: %q %v", output, err)
		}
	}
	waitReady()
	assertAbsent()
	options := kamalproxy.Options{ContainerName: proxyName, Network: network}
	fresh := kamalproxy.New(rt, options)
	if err := fresh.SetTargets(ctx, "app", "web", nil, proxy.RouteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, restartProxy := range []bool{false, true} {
		manager := kamalproxy.New(rt, options)
		target := proxy.Target{Service: "app", Role: "web", ContainerName: appName, Port: 8080, HealthPath: "/"}
		if err := manager.SetTargets(ctx, "app", "web", []proxy.Target{target}, proxy.RouteOptions{}); err != nil {
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
		if err != nil || !strings.Contains(output, "app-web") {
			t.Fatalf("route absent before removal: %q %v", output, err)
		}
		fresh = kamalproxy.New(rt, options)
		for i := 0; i < 2; i++ {
			if err := fresh.SetTargets(ctx, "app", "web", nil, proxy.RouteOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		assertAbsent()
	}
}
