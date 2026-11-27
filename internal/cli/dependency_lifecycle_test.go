package cli_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/runtime"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

const dependencyLifecycleConfig = `service: my-app
image: busybox:1.36
destination: production
servers:
  web:
    hosts: [localhost]
    command: [sleep, "3600"]
dependencies:
  postgres:
    image: postgres:16-alpine
    hosts: [localhost]
    internal_port: 5432
    publish:
      host_port: 15432
    volumes:
      - postgres-data:/var/lib/postgresql/data
`

func runningDependencies(t *testing.T, rt *fake.Runtime) []runtime.ContainerState {
	t.Helper()
	var running []runtime.ContainerState
	for _, container := range listManagedContainers(t, rt) {
		if container.Labels["serve.container_type"] == "accessory" && container.Running {
			running = append(running, container)
		}
	}
	return running
}

func TestDeployOfNewVersionKeepsUnchangedDependencyRunning(t *testing.T) {
	rt := fake.NewRuntime()
	stateDir := t.TempDir()
	configPath := filepath.Join(stateDir, "serve.yml")
	if err := os.WriteFile(configPath, []byte(dependencyLifecycleConfig), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cmd := socketCommand(t, rt, stateDir)
	deploy := func(version string) {
		t.Helper()
		var stderr bytes.Buffer
		exitCode := cmd.Run(context.Background(), []string{
			"deploy", "--local", "--config", configPath, "--host", "localhost", "--version", version,
		}, io.Discard, &stderr)
		if exitCode != 0 {
			t.Fatalf("deploy %s exit code = %d: %s", version, exitCode, stderr.String())
		}
	}

	deploy("v1")
	before := runningDependencies(t, rt)
	if len(before) != 1 {
		t.Fatalf("running dependencies after first deploy = %#v", before)
	}
	deploy("v2")

	after := runningDependencies(t, rt)
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("dependency was replaced by an application-only deploy: before %#v, after %#v", before, after)
	}
	for _, container := range listManagedContainers(t, rt) {
		if container.Labels["serve.container_type"] == "app" && container.Running && container.Labels["serve.version"] != "v2" {
			t.Fatalf("application did not move to v2: %#v", container)
		}
	}

	var stdout bytes.Buffer
	if exitCode := cmd.Run(context.Background(), []string{"status"}, &stdout, io.Discard); exitCode != 0 {
		t.Fatalf("status exit code = %d", exitCode)
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if !strings.Contains(line, after[0].Name) {
			continue
		}
		if fields := strings.Fields(line); len(fields) != 6 || fields[3] != "-" {
			t.Fatalf("dependency status line must show no application version, got %q", line)
		}
		return
	}
	t.Fatalf("status does not list the dependency:\n%s", stdout.String())
}
