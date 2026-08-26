package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/config"
)

func TestLoadMissingFileReturnsClearError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.yml")

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected missing config file error, got nil")
	}

	message := err.Error()
	if !strings.Contains(message, "config file not found") {
		t.Fatalf("expected error to explain the file is missing, got %q", message)
	}
	if !strings.Contains(message, path) {
		t.Fatalf("expected error to include missing path %q, got %q", path, message)
	}
}

func TestLoadServicesExpandsMultiServiceManifest(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
destination: production
networking:
  private_network: serve
retain_containers: 3
services:
  api:
    image: ghcr.io/acme/api
    servers:
      web:
        hosts: [app.example.com]
  worker:
    image: ghcr.io/acme/worker
    servers:
      jobs:
        hosts: [worker.example.com]
`)

	services, err := config.LoadServices(path)

	if err != nil {
		t.Fatalf("expected valid multi-service config, got error: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("expected two services, got %#v", services)
	}
	if services[0].Service != "api" || services[0].Image != "ghcr.io/acme/api" {
		t.Fatalf("first service = %#v", services[0])
	}
	if services[1].Service != "worker" || services[1].Image != "ghcr.io/acme/worker" {
		t.Fatalf("second service = %#v", services[1])
	}
	for _, service := range services {
		if service.Destination != "production" || service.Networking.PrivateNetwork != "serve" || service.RetainContainers != 3 {
			t.Fatalf("service did not inherit global settings: %#v", service)
		}
	}
}

func TestLoadServicesRejectsEmptyServiceMap(t *testing.T) {
	path := writeConfig(t, "serve.yml", "services: {}\n")

	_, err := config.LoadServices(path)

	if err == nil || !strings.Contains(err.Error(), "at least one service") {
		t.Fatalf("LoadServices error = %v, want empty services error", err)
	}
}

func TestLoadServicesRejectsUnknownTopLevelField(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
services:
  api:
    image: ghcr.io/acme/api
unknown: value
`)

	_, err := config.LoadServices(path)

	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("LoadServices error = %v, want unknown field error", err)
	}
}

func TestLoadServicesRejectsLegacyApplicationFieldsBesideServices(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
image: ghcr.io/acme/legacy
services:
  api:
    image: ghcr.io/acme/api
`)

	_, err := config.LoadServices(path)

	if err == nil || !strings.Contains(err.Error(), "image cannot be configured beside services") {
		t.Fatalf("LoadServices error = %v, want mixed schema error", err)
	}
}

func TestLoadServicesRejectsServiceLevelGlobalSettings(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
destination: production
services:
  api:
    image: ghcr.io/acme/api
    destination: staging
`)

	_, err := config.LoadServices(path)

	if err == nil || !strings.Contains(err.Error(), "services.api.destination must be configured at the top level") {
		t.Fatalf("LoadServices error = %v, want service-level global setting error", err)
	}
}

func TestLoadServicesRejectsDuplicateProxyHosts(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
services:
  api:
    image: ghcr.io/acme/api
    proxy:
      hosts: [app.example.com]
  admin:
    image: ghcr.io/acme/admin
    proxy:
      hosts: [app.example.com]
`)

	_, err := config.LoadServices(path)

	if err == nil || !strings.Contains(err.Error(), `proxy host "app.example.com" is configured by both admin and api`) {
		t.Fatalf("LoadServices error = %v, want duplicate proxy host error", err)
	}
}

func TestLoadAcceptsMinimalValidConfig(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.Service != "my-app" {
		t.Fatalf("expected service %q, got %q", "my-app", cfg.Service)
	}
	if cfg.Image != "ghcr.io/acme/my-app" {
		t.Fatalf("expected image %q, got %q", "ghcr.io/acme/my-app", cfg.Image)
	}
}

func TestLoadRejectsRemovedRegistryConfiguration(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
registry:
  server: ghcr.io
  username: deploy
  password:
    - GHCR_TOKEN
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "field registry not found") {
		t.Fatalf("Load error = %v, want removed registry field error", err)
	}
}

func TestLoadRejectsMissingService(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
image: ghcr.io/acme/my-app
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "service is required") {
		t.Fatalf("expected missing service error, got %q", err.Error())
	}
}

func TestLoadRejectsMissingImage(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("expected missing image error, got %q", err.Error())
	}
}

func TestLoadRejectsUnsafeIdentifiers(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
		want   string
	}{
		{"service", "service: ../escape\nimage: app\n", "service"},
		{"destination", "service: app\nimage: app\ndestination: prod/blue\n", "destination"},
		{"role", "service: app\nimage: app\nservers:\n  web/api: {}\n", "servers.web/api"},
		{"accessory", "service: app\nimage: app\naccessories:\n  ../db:\n    image: postgres\n", "accessories.../db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, "serve.yml", test.config))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want identifier error containing %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsSSHOptionAsServerHost(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    hosts:
      - -oProxyCommand=touch /tmp/local-pwned
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "servers.web.hosts") {
		t.Fatalf("Load error = %v, want unsafe host validation error", err)
	}
}

func TestLoadParsesServerVolumes(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    hosts: [app.example.com]
    volumes:
      - app-uploads:/app/uploads
      - /srv/app/config.yml:/app/config.yml:ro
      - app-cache:/app/cache:rw
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid server volumes, got error: %v", err)
	}
	volumes := cfg.Servers["web"].Volumes
	want := []string{"app-uploads:/app/uploads", "/srv/app/config.yml:/app/config.yml:ro", "app-cache:/app/cache:rw"}
	if !reflect.DeepEqual(volumes, want) {
		t.Fatalf("server volumes = %#v, want %#v", volumes, want)
	}
}

func TestLoadRejectsServerVolumeWithoutContainerTarget(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    volumes:
      - app-uploads
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "servers.web.volumes") {
		t.Fatalf("Load error = %v, want invalid server volume error", err)
	}
}

func TestLoadRejectsServerVolumeWithRelativeContainerTarget(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    volumes:
      - app-uploads:data
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "container target must be absolute") {
		t.Fatalf("Load error = %v, want absolute volume target error", err)
	}
}

func TestLoadRejectsRelativeBindMountSource(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    volumes:
      - ./data:/app/data
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "source must be an absolute host path or a valid volume name") {
		t.Fatalf("Load error = %v, want invalid volume source error", err)
	}
}

func TestLoadRejectsUnsupportedServerVolumeOption(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    volumes:
      - app-uploads:/app/uploads:cached
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "option must be ro or rw") {
		t.Fatalf("Load error = %v, want unsupported volume option error", err)
	}
}

func TestLoadRejectsDuplicateServerVolumeTargets(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    volumes:
      - app-data:/app/data
      - legacy-data:/app/data:ro
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), `target "/app/data" is configured more than once`) {
		t.Fatalf("Load error = %v, want duplicate volume target error", err)
	}
}

func TestLoadParsesServerAliases(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  api:
    hosts: [app.example.com]
    aliases:
      - api
      - api.internal
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid server aliases, got error: %v", err)
	}
	aliases := cfg.Servers["api"].Aliases
	if len(aliases) != 2 || aliases[0] != "api" || aliases[1] != "api.internal" {
		t.Fatalf("server aliases = %#v, want api and api.internal", aliases)
	}
}

func TestLoadRejectsInvalidOrConflictingAliases(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
		want   string
	}{
		{
			name: "empty",
			config: `service: app
image: app
servers:
  api:
    aliases: [""]
`,
			want: "servers.api.aliases",
		},
		{
			name: "invalid",
			config: `service: app
image: app
servers:
  api:
    aliases: [api/unsafe]
`,
			want: "servers.api.aliases",
		},
		{
			name: "duplicate across roles",
			config: `service: app
image: app
servers:
  api:
    aliases: [backend]
  voice:
    aliases: [backend]
`,
			want: `alias "backend" is also used by servers.api`,
		},
		{
			name: "conflicts with accessory",
			config: `service: app
image: app
servers:
  api:
    aliases: [backend]
accessories:
  redis:
    image: redis:7
    aliases: [backend]
`,
			want: `alias "backend" is also used by servers.api`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, "serve.yml", test.config))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want alias error containing %q", err, test.want)
			}
		})
	}
}

func TestLoadAcceptsStandardSSHDestinations(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    hosts:
      - deploy@app.example.com
      - host_alias
      - "[2001:db8::1]"
`)

	if _, err := config.Load(path); err != nil {
		t.Fatalf("expected standard SSH destinations to validate: %v", err)
	}
}

func TestLoadRejectsInvalidHealthPolicyNumbers(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    healthcheck:
      interval: -1s
      timeout: -2s
      retries: -1
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected invalid health policy error")
	}
	for _, want := range []string{"healthcheck.interval", "healthcheck.timeout", "healthcheck.retries"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadRejectsInvalidRestartPolicy(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
servers:
  web:
    restart:
      policy: sometimes
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "servers.web.restart.policy") {
		t.Fatalf("expected error to identify invalid restart policy path, got %q", err.Error())
	}
}

func TestLoadRejectsAgentRestartControlsWithDockerController(t *testing.T) {
	for _, field := range []string{"initial_backoff", "max_backoff", "window"} {
		t.Run(field, func(t *testing.T) {
			path := writeConfig(t, "serve.yml", fmt.Sprintf(`
service: app
image: app
servers:
  web:
    restart:
      controller: docker
      policy: always
      %s: 1s
`, field))

			_, err := config.Load(path)

			want := field + " is only supported by the agent restart controller"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Load error = %v, want %q", err, want)
			}
		})
	}
}

func TestLoadRejectsDockerMaxAttemptsWithoutOnFailurePolicy(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    restart:
      controller: docker
      policy: always
      max_attempts: 3
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "max_attempts requires policy on-failure") {
		t.Fatalf("Load error = %v, want unsupported Docker max attempts error", err)
	}
}

func TestLoadRejectsNegativeRestartControls(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    restart:
      initial_backoff: -1s
      max_backoff: -2s
      max_attempts: -3
      window: -4s
`)

	_, err := config.Load(path)

	if err == nil {
		t.Fatal("expected negative restart controls to be rejected")
	}
	for _, field := range []string{"initial_backoff", "max_backoff", "max_attempts", "window"} {
		if !strings.Contains(err.Error(), "servers.web.restart."+field) {
			t.Fatalf("error %q does not mention restart field %s", err, field)
		}
	}
}

func TestLoadRejectsNegativeReplicas(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  web:
    replicas: -1
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "servers.web.replicas") {
		t.Fatalf("Load error = %v, want negative replicas error", err)
	}
}

func TestLoadParsesDependencies(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
dependencies:
  database:
    image: postgres:16-alpine
    hosts: [app.example.com]
    aliases: [database]
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected dependencies to load, got error: %v", err)
	}
	dependency, ok := cfg.Dependencies["database"]
	if !ok || dependency.Image != "postgres:16-alpine" {
		t.Fatalf("database dependency = %#v", dependency)
	}
}

func TestLoadDefaultsDependencyPublishHostIPToLoopback(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
dependencies:
  redis:
    image: redis:8-alpine
    internal_port: 6379
    publish:
      host_port: 16379
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid dependency publication, got error: %v", err)
	}
	publish := cfg.Dependencies["redis"].Publish
	if publish == nil {
		t.Fatal("expected dependency publish configuration")
	}
	if publish.HostPort != 16379 || publish.HostIP != "127.0.0.1" {
		t.Fatalf("dependency publish = %#v, want host port 16379 on loopback", publish)
	}
}

func TestLoadParsesExplicitDependencyPublishHostIP(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
dependencies:
  redis:
    image: redis:8-alpine
    internal_port: 6379
    publish:
      host_port: 6379
      host_ip: 10.0.0.5
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid dependency publication, got error: %v", err)
	}
	publish := cfg.Dependencies["redis"].Publish
	if publish == nil || publish.HostPort != 6379 || publish.HostIP != "10.0.0.5" {
		t.Fatalf("dependency publish = %#v, want explicit private interface binding", publish)
	}
}

func TestLoadRejectsInvalidDependencyPublishConfiguration(t *testing.T) {
	for _, test := range []struct {
		name       string
		portFields string
		want       string
	}{
		{name: "missing internal port", portFields: "publish:\n      host_port: 6379", want: "internal_port must be between 1 and 65535 when publish is configured"},
		{name: "negative internal port", portFields: "internal_port: -1", want: "internal_port must be between 1 and 65535"},
		{name: "out of range internal port", portFields: "internal_port: 65536", want: "internal_port must be between 1 and 65535"},
		{name: "missing host port", portFields: "internal_port: 6379\n    publish: {}", want: "publish.host_port must be between 1 and 65535"},
		{name: "negative host port", portFields: "internal_port: 6379\n    publish:\n      host_port: -1", want: "publish.host_port must be between 1 and 65535"},
		{name: "out of range host port", portFields: "internal_port: 6379\n    publish:\n      host_port: 65536", want: "publish.host_port must be between 1 and 65535"},
		{name: "invalid host IP", portFields: "internal_port: 6379\n    publish:\n      host_port: 6379\n      host_ip: private-interface", want: "publish.host_ip must be a valid IP address"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, "serve.yml", "service: app\nimage: app\ndependencies:\n  redis:\n    image: redis:8-alpine\n    "+test.portFields+"\n")

			_, err := config.Load(path)

			if err == nil || !strings.Contains(err.Error(), "dependencies.redis."+test.want) {
				t.Fatalf("Load error = %v, want dependency publication error containing %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidDependencyVolume(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
dependencies:
  database:
    image: postgres:16-alpine
    volumes:
      - database-data
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "dependencies.database.volumes") {
		t.Fatalf("Load error = %v, want invalid dependency volume error", err)
	}
}

func TestLoadRejectsDependenciesAndAccessoriesTogether(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
dependencies: {}
accessories: {}
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "dependencies and accessories cannot both be configured") {
		t.Fatalf("Load error = %v, want conflicting field error", err)
	}
}

func TestLoadRejectsRoleAndDependencyWithSameName(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
servers:
  database:
    hosts: [app.example.com]
dependencies:
  database:
    image: postgres:16
    hosts: [app.example.com]
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "database cannot be both a server role and a dependency") {
		t.Fatalf("Load error = %v, want role/dependency collision error", err)
	}
}

func TestLoadRequiresAccessoryImage(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
accessories:
  database:
    hosts: [app.example.com]
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "accessories.database.image") {
		t.Fatalf("Load error = %v, want missing accessory image error", err)
	}
}

func TestLoadParsesAccessoryEnvironment(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
accessories:
  database:
    image: postgres:16-alpine
    hosts: [app.example.com]
    env:
      plain:
        POSTGRES_USER: app
        POSTGRES_DB: app_production
      secret:
        - POSTGRES_PASSWORD
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid accessory environment, got error: %v", err)
	}
	env := cfg.Accessories["database"].Env
	if env.Plain["POSTGRES_USER"] != "app" || env.Plain["POSTGRES_DB"] != "app_production" {
		t.Fatalf("accessory plain environment = %#v", env.Plain)
	}
	if len(env.Secret) != 1 || env.Secret[0] != "POSTGRES_PASSWORD" {
		t.Fatalf("accessory secret environment = %#v", env.Secret)
	}
}

func TestLoadRejectsClearEnvironmentField(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: app
image: app
env:
  clear:
    RACK_ENV: production
`)

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "field clear not found") {
		t.Fatalf("Load error = %v, want removed env.clear field error", err)
	}
}

func TestLoadDefaultsRestartControllerToAgent(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
servers:
  web:
    restart:
      policy: always
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.Servers["web"].Restart.Controller != "agent" {
		t.Fatalf("expected restart controller %q, got %q", "agent", cfg.Servers["web"].Restart.Controller)
	}
}

func TestLoadDefaultsPrivateNetworkAndRetainContainers(t *testing.T) {
	path := writeConfig(t, "serve.yml", `
service: my-app
image: ghcr.io/acme/my-app
`)

	cfg, err := config.Load(path)

	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.Networking.PrivateNetwork != "serve" {
		t.Fatalf("expected private network %q, got %q", "serve", cfg.Networking.PrivateNetwork)
	}
	if cfg.RetainContainers != 5 {
		t.Fatalf("expected retain_containers %d, got %d", 5, cfg.RetainContainers)
	}
}

func TestLoadMergesDestinationOverlay(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "serve.yml")
	writeFile(t, basePath, `
service: my-app
image: ghcr.io/acme/my-app
destination: production
servers:
  web:
    hosts:
      - base.example.com
    restart:
      policy: always
env:
  plain:
    RACK_ENV: staging
`)
	writeFile(t, filepath.Join(dir, "serve.production.yml"), `
servers:
  web:
    hosts:
      - prod.example.com
    restart:
      max_attempts: 3
env:
  plain:
    RACK_ENV: production
    FEATURE_FLAG: enabled
`)

	cfg, err := config.Load(basePath, config.WithDestination("production"))

	if err != nil {
		t.Fatalf("expected valid merged config, got error: %v", err)
	}
	web := cfg.Servers["web"]
	if len(web.Hosts) != 1 || web.Hosts[0] != "prod.example.com" {
		t.Fatalf("expected overlay hosts to replace base hosts, got %#v", web.Hosts)
	}
	if web.Restart.Policy != "always" {
		t.Fatalf("expected base restart policy to be preserved, got %q", web.Restart.Policy)
	}
	if web.Restart.MaxAttempts != 3 {
		t.Fatalf("expected overlay restart max attempts 3, got %d", web.Restart.MaxAttempts)
	}
	if cfg.Env.Plain["RACK_ENV"] != "production" {
		t.Fatalf("expected overlay env value, got %q", cfg.Env.Plain["RACK_ENV"])
	}
	if cfg.Env.Plain["FEATURE_FLAG"] != "enabled" {
		t.Fatalf("expected overlay env key to be merged, got %q", cfg.Env.Plain["FEATURE_FLAG"])
	}
}

func writeConfig(t *testing.T, name string, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeFile(t, path, contents)
	return path
}

func writeFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.TrimPrefix(contents, "\n")), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}
