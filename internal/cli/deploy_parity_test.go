package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/agent/daemon"
	"github.com/uptimenine/serve/internal/agent/health"
	"github.com/uptimenine/serve/internal/agent/secrets"
	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/cli"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

// The SSH boundary runs the same receiving CLI as a production SSH command,
// but against the test agent. No deployment/apply behavior is stubbed.
type loopbackSSH struct {
	command agentCommand
	dir     string
}

func (s loopbackSSH) Run(ctx context.Context, host, command string, stdin io.Reader, stdout io.Writer) error {
	if command != "sudo serve agent apply /dev/stdin --socket /run/serve/agent.sock" {
		return fmt.Errorf("unexpected SSH command: %s", command)
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "remote-desired.json")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		return err
	}
	var stderr bytes.Buffer
	if s.command.Run(ctx, []string{"agent", "apply", path}, io.Discard, &stderr) != 0 {
		return fmt.Errorf("%s", &stderr)
	}
	return nil
}

type versionHealth struct{}

func (versionHealth) Check(ctx context.Context, target health.Target) (health.Status, error) {
	if strings.Contains(target.ContainerName, "bad") {
		return health.Unhealthy, nil
	}
	return health.Healthy, nil
}

type ciphertextStore struct{ encrypted string }

func (s ciphertextStore) Resolve(ctx context.Context, request secrets.Request) (map[string]string, error) {
	if request.EncryptedFile != s.encrypted {
		return nil, fmt.Errorf("encrypted secret payload lost")
	}
	return map[string]string{"TOKEN": "resolved-only-on-agent"}, nil
}
func TestDeployTransportsShareCutoverSecretsStateAndFailureHandling(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("local=%t", local), func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			envDir := filepath.Join(dir, "env")
			config := filepath.Join(dir, "serve.yml")
			contents := `service: app
image: example/app
destination: production
servers:
  web:
    hosts: [deploy@app.example.com]
    aliases: [api]
    app_port: 3000
    healthcheck:
      http:
        path: /up
        port: 3000
      retries: 1
      interval: 1ms
    restart:
      policy: always
      controller: agent
env:
  secret: [TOKEN]
`
			if err := os.WriteFile(config, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			encrypted := "TOKEN: ENC[AES256_GCM,data:token-ciphertext]\nsops:\n  kms: []\n"
			if err := os.WriteFile(filepath.Join(dir, "serve.secrets.yml"), []byte(encrypted), 0600); err != nil {
				t.Fatal(err)
			}
			rt := fake.NewRuntime()
			agent := socketCommand(t, rt, stateDir, func(cfg *daemon.Config) {
				cfg.HealthChecker = versionHealth{}
				cfg.SecretStore = ciphertextStore{encrypted}
				cfg.EnvFileDir = envDir
			})
			remote := cli.New("test", cli.WithSSHRunner(loopbackSSH{command: agent, dir: dir}))
			deploy := func(version string) (int, string) {
				args := []string{"deploy", "--config", config, "--version", version}
				var out, errOut bytes.Buffer
				if local {
					args = append(args, "--local", "--host", "deploy@app.example.com")
					return agent.Run(context.Background(), args, &out, &errOut), errOut.String()
				}
				return remote.Run(context.Background(), args, &out, &errOut), errOut.String()
			}
			for _, version := range []string{"v1", "v2"} {
				if code, err := deploy(version); code != 0 {
					t.Fatalf("deploy %s: %d %s", version, code, err)
				}
			}
			if code, err := deploy("bad"); code != 1 || !strings.Contains(err, "health") {
				t.Fatalf("unhealthy candidate: %d %s", code, err)
			}
			store := agentstate.NewStore(stateDir)
			desired, err := store.LoadDesired("app", "production")
			if err != nil {
				t.Fatal(err)
			}
			if desired.Version != "v2" || desired.SecretsFile != encrypted {
				t.Fatalf("active state changed on failure: %#v", desired)
			}
			lastGood, err := store.LoadLastGood("app", "production")
			if err != nil {
				t.Fatal(err)
			}
			// Existing cutover semantics preserve the current successful version before attempting a new one.
			if lastGood.Version != "v2" {
				t.Fatalf("last-good=%s", lastGood.Version)
			}
			actual, err := store.LoadActual("app", "production")
			if err != nil {
				t.Fatal(err)
			}
			running := 0
			for _, c := range actual.Containers {
				if c.Status == "running" && c.Role == "web" {
					running++
					if c.Version != "v2" {
						t.Fatalf("actual version=%s", c.Version)
					}
				}
			}
			if running != 1 {
				t.Fatalf("running apps=%d", running)
			}
			for _, c := range listManagedContainers(t, rt) {
				if strings.Contains(c.Name, "bad") {
					t.Fatalf("failed candidate not cleaned up: %s", c.Name)
				}
				if c.Running && c.Labels["serve.role"] == "web" && (len(c.Aliases) != 1 || c.Aliases[0] != "api") {
					t.Fatalf("active aliases lost: %#v", c)
				}
			}
			entries, err := os.ReadDir(envDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatal("plaintext env files leaked")
			}
		})
	}
}
