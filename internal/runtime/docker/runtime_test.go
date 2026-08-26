package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	registrytypes "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	serveruntime "github.com/uptimenine/serve/internal/runtime"
)

func TestCreateContainerUsesTypedVolumeAndBindMounts(t *testing.T) {
	var request containertypes.CreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode create request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"Id":"container-id","Warnings":[]}`)
	}))
	t.Cleanup(server.Close)

	_, err := New(dockerClientForServer(t, server)).CreateContainer(context.Background(), serveruntime.ContainerSpec{
		Name:    "app",
		Image:   "busybox:1.36",
		Volumes: []string{"app-data:/app/data", "/srv/app/config:/app/config:ro"},
	})

	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	if request.HostConfig == nil {
		t.Fatal("create request is missing host config")
	}
	want := []mount.Mount{
		{Type: mount.TypeVolume, Source: "app-data", Target: "/app/data"},
		{Type: mount.TypeBind, Source: "/srv/app/config", Target: "/app/config", ReadOnly: true},
	}
	if !reflect.DeepEqual(request.HostConfig.Mounts, want) {
		t.Fatalf("Docker mounts = %#v, want %#v", request.HostConfig.Mounts, want)
	}
	if len(request.HostConfig.Binds) != 0 {
		t.Fatalf("Docker legacy binds = %#v, want typed mounts only", request.HostConfig.Binds)
	}
}

func TestCreateContainerPublishesConfiguredHostPort(t *testing.T) {
	var request containertypes.CreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode create request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"Id":"container-id","Warnings":[]}`)
	}))
	t.Cleanup(server.Close)

	_, err := New(dockerClientForServer(t, server)).CreateContainer(context.Background(), serveruntime.ContainerSpec{
		Name:  "redis",
		Image: "redis:8-alpine",
		Ports: []serveruntime.Port{{Name: "tcp", ContainerPort: 6379, HostPort: 16379, HostIP: "10.0.0.5"}},
	})

	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	if request.HostConfig == nil {
		t.Fatal("create request is missing host config")
	}
	bindings := request.HostConfig.PortBindings[nat.Port("6379/tcp")]
	want := []nat.PortBinding{{HostIP: "10.0.0.5", HostPort: "16379"}}
	if !reflect.DeepEqual(bindings, want) {
		t.Fatalf("Docker port bindings = %#v, want %#v", bindings, want)
	}
}

func TestPullImageUsesCredentialsFromDockerConfig(t *testing.T) {
	configDir := t.TempDir()
	writeDockerConfig(t, configDir, dockerAuthConfig("ghcr.io", "deploy", "secret-token"))
	t.Setenv("DOCKER_CONFIG", configDir)

	resolved := pullAuth(t, "ghcr.io/acme/app:v1")

	if resolved.Username != "deploy" || resolved.Password != "secret-token" {
		t.Fatalf("registry auth = %#v, want Docker config credentials", resolved)
	}
}

func TestPullImageUsesCredentialsFromHomeDockerConfig(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".docker")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatalf("create Docker config directory: %v", err)
	}
	writeDockerConfig(t, configDir, dockerAuthConfig("ghcr.io", "home-user", "home-token"))
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("HOME", home)

	resolved := pullAuth(t, "ghcr.io/acme/app:v1")

	if resolved.Username != "home-user" || resolved.Password != "home-token" {
		t.Fatalf("registry auth = %#v, want credentials from ~/.docker/config.json", resolved)
	}
}

func TestPullImageUsesDockerHubCredentials(t *testing.T) {
	configDir := t.TempDir()
	writeDockerConfig(t, configDir, dockerAuthConfig("https://index.docker.io/v1/", "hub-user", "hub-token"))
	t.Setenv("DOCKER_CONFIG", configDir)

	resolved := pullAuth(t, "busybox:1.36")

	if resolved.Username != "hub-user" || resolved.Password != "hub-token" {
		t.Fatalf("registry auth = %#v, want Docker Hub credentials", resolved)
	}
}

func TestPullImageUsesDockerCredentialHelper(t *testing.T) {
	configDir := t.TempDir()
	writeDockerConfig(t, configDir, `{"credHelpers":{"ghcr.io":"serve-test"}}`)
	helperDir := t.TempDir()
	helperPath := filepath.Join(helperDir, "docker-credential-serve-test")
	helper := `#!/bin/sh
read server
if [ "$1" != "get" ] || [ "$server" != "ghcr.io" ]; then
  exit 1
fi
printf '{"Username":"helper-user","Secret":"helper-token"}'
`
	if err := os.WriteFile(helperPath, []byte(helper), 0o700); err != nil {
		t.Fatalf("write credential helper: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", configDir)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resolved := pullAuth(t, "ghcr.io/acme/app:v1")

	if resolved.Username != "helper-user" || resolved.Password != "helper-token" {
		t.Fatalf("registry auth = %#v, want credential helper credentials", resolved)
	}
}

func TestPullImageReturnsErrorFromDockerPullStream(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"errorDetail":{"message":"unauthorized: authentication required"},"error":"unauthorized: authentication required"}`)
	}))
	t.Cleanup(server.Close)
	cli := dockerClientForServer(t, server)

	err := New(cli).PullImage(context.Background(), "ghcr.io/acme/app:v1")

	if err == nil || !strings.Contains(err.Error(), "unauthorized: authentication required") {
		t.Fatalf("PullImage error = %v, want streamed registry error", err)
	}
}

func pullAuth(t *testing.T, imageRef string) *registrytypes.AuthConfig {
	t.Helper()
	var encodedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodedAuth = r.Header.Get(registrytypes.AuthHeader)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"pulled"}`)
	}))
	t.Cleanup(server.Close)
	cli := dockerClientForServer(t, server)

	if err := New(cli).PullImage(context.Background(), imageRef); err != nil {
		t.Fatalf("pull image: %v", err)
	}
	resolved, err := registrytypes.DecodeAuthConfig(encodedAuth)
	if err != nil {
		t.Fatalf("decode registry auth header: %v", err)
	}
	return resolved
}

func dockerClientForServer(t *testing.T, server *httptest.Server) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"))
	if err != nil {
		t.Fatalf("create Docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func writeDockerConfig(t *testing.T, configDir string, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write Docker config: %v", err)
	}
}

func dockerAuthConfig(registry string, username string, password string) string {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registry, auth)
}
