package cli_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/agent/daemon"
	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/cli"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestFailedRollbackPreservesLifecycleOutputAndNonzeroExit(t *testing.T) {
	rt := fake.NewRuntime()
	dir := t.TempDir()
	desired := desiredState("bad")
	desired.Containers[0].Proxy = true
	desired.Containers[0].Ports = []planner.Port{{ContainerPort: 3000}}
	desired.Containers[0].Healthcheck = &planner.Healthcheck{Type: "http", Path: "/up", Port: 3000, Retries: 1}
	if err := agentstate.NewStore(dir).SaveLastGood(desired); err != nil {
		t.Fatal(err)
	}
	cmd := socketCommand(t, rt, dir, func(cfg *daemon.Config) { cfg.HealthChecker = versionHealth{} })
	var out, errOut bytes.Buffer
	code := cmd.Run(context.Background(), []string{"rollback", "--service", "my-app", "--destination", "production"}, &out, &errOut)
	if code != 1 || !strings.Contains(out.String(), "rollback_started") || !strings.Contains(out.String(), "rollback_failed") || strings.Contains(out.String(), "rollback_completed") || !strings.Contains(errOut.String(), "health") {
		t.Fatalf("failed rollback: code=%d out=%s err=%s", code, &out, &errOut)
	}
}
func TestRollbackRequiresAnExplicitSuccessfulOutcome(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"failed","error":"apply failed","output":"rollback_started\n"}`, `{"status":"completed","error":"persistence failed"}`} {
		socket := startStubAgentSocket(t, map[string]string{"POST /v1/rollback": body})
		var errOut bytes.Buffer
		code := cli.New("test").Run(context.Background(), []string{"rollback", "--socket", socket, "--service", "app", "--destination", "production"}, io.Discard, &errOut)
		if code != 1 {
			t.Fatalf("ambiguous or failed response reported success: %s", body)
		}
	}
}
