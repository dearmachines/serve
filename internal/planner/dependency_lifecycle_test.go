package planner_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/config"
	"github.com/uptimenine/serve/internal/planner"
)

func dependencyLifecycleConfig() config.Config {
	cfg := baseConfig()
	cfg.Servers = map[string]config.ServerConfig{
		"web": {Hosts: []string{"app1.example.com"}, AppPort: 3000},
	}
	cfg.Dependencies = map[string]config.DependencyConfig{
		"postgres": {
			Image:        "postgres:16-alpine",
			Hosts:        []string{"app1.example.com"},
			InternalPort: 5432,
			Volumes:      []string{"postgres-data:/var/lib/postgresql/data"},
		},
	}
	return cfg
}

func planDependency(t *testing.T, cfg config.Config, version string) planner.Container {
	t.Helper()
	state, err := planner.Plan(cfg, planner.Options{Host: "app1.example.com", Version: version})
	if err != nil {
		t.Fatalf("plan %s: %v", version, err)
	}
	for _, container := range state.Containers {
		if container.ContainerType == "accessory" {
			return container
		}
	}
	t.Fatalf("no dependency planned for %s: %#v", version, state.Containers)
	return planner.Container{}
}

func TestPlanKeepsUnchangedDependencyIdentityAcrossVersions(t *testing.T) {
	cfg := dependencyLifecycleConfig()

	first := planDependency(t, cfg, "abc123")
	second := planDependency(t, cfg, "def456")

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("unchanged dependency differs between versions:\n%#v\n%#v", first, second)
	}
	if _, versioned := first.Labels["serve.version"]; versioned {
		t.Fatalf("dependency must not carry the application version label: %#v", first.Labels)
	}
	if strings.Contains(first.Name, "abc123") {
		t.Fatalf("dependency name %q contains the application version", first.Name)
	}
}

func TestPlanNamesDependencyByItsSpecHash(t *testing.T) {
	cfg := dependencyLifecycleConfig()

	first := planDependency(t, cfg, "abc123")
	hash := first.Labels["serve.spec_hash"]
	if len(hash) < 12 {
		t.Fatalf("dependency spec hash = %q", hash)
	}
	if want := "my-app-postgres-production-" + hash[:12] + "-r1"; first.Name != want {
		t.Fatalf("dependency name = %q, want %q", first.Name, want)
	}

	dependency := cfg.Dependencies["postgres"]
	dependency.Image = "postgres:17-alpine"
	cfg.Dependencies["postgres"] = dependency
	changed := planDependency(t, cfg, "abc123")

	if changed.Name == first.Name {
		t.Fatalf("changed dependency kept name %q", first.Name)
	}
}

func TestPlanKeepsSecretBearingDependencyIdentityUntilItsSecretChanges(t *testing.T) {
	cfg := dependencyLifecycleConfig()
	cfg.Env.Secret = []string{"API_TOKEN"}
	dependency := cfg.Dependencies["postgres"]
	dependency.Env = config.EnvConfig{Secret: []string{"POSTGRES_PASSWORD"}}
	cfg.Dependencies["postgres"] = dependency
	plan := func(version string, secretsFile string) planner.Container {
		t.Helper()
		state, err := planner.Plan(cfg, planner.Options{Host: "app1.example.com", Version: version, SecretsFileContent: secretsFile})
		if err != nil {
			t.Fatalf("plan %s: %v", version, err)
		}
		return state.Containers[len(state.Containers)-1]
	}
	secrets := "API_TOKEN: ENC[AES256_GCM,data:token1]\nPOSTGRES_PASSWORD: ENC[AES256_GCM,data:password1]\n"

	first := plan("abc123", secrets)
	second := plan("def456", secrets)
	unrelated := plan("def456", strings.Replace(secrets, "token1", "token2", 1))
	rotated := plan("def456", strings.Replace(secrets, "password1", "password2", 1))

	if first.ContainerType != "accessory" || len(first.SecretNames) != 1 {
		t.Fatalf("expected the secret-bearing dependency, got %#v", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("secret-bearing dependency differs between versions:\n%#v\n%#v", first, second)
	}
	if unrelated.Name != first.Name {
		t.Fatalf("rotating an application secret renamed the dependency: %q -> %q", first.Name, unrelated.Name)
	}
	if rotated.Name == first.Name {
		t.Fatalf("rotating the dependency's own secret kept name %q", first.Name)
	}
}

func TestPlanKeepsApplicationContainersVersioned(t *testing.T) {
	state, err := planner.Plan(dependencyLifecycleConfig(), planner.Options{Host: "app1.example.com", Version: "abc123"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	web := state.Containers[0]
	if web.Name != "my-app-web-production-abc123-r1" || web.Labels["serve.version"] != "abc123" {
		t.Fatalf("application container lost its version identity: %#v", web)
	}
}
