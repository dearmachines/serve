package cli_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/cli"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

func TestRemoteCommandsRejectLocalSocketOverride(t *testing.T) {
	for _, args := range [][]string{
		{"deploy"}, {"status", "--config", "serve.yml"},
		{"logs", "--host", "app.example.com", "--container", "app"},
		{"events", "--host", "app.example.com", "--once"},
		{"exec", "--host", "app.example.com", "--container", "app", "--", "echo", "hi"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			ssh := &recordingSSHRunner{}
			cmd := agentCommand{command: cli.New("test", cli.WithSSHRunner(ssh)), socket: "/tmp/local.sock"}
			var stderr bytes.Buffer
			if code := cmd.Run(context.Background(), args, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "--socket") {
				t.Fatalf("expected socket conflict, got %d: %s", code, &stderr)
			}
			if len(ssh.calls) != 0 {
				t.Fatal("invalid socket override reached SSH")
			}
		})
	}
}
func TestStateDirectoryBelongsOnlyToAgentRun(t *testing.T) {
	for _, args := range [][]string{{"deploy", "--local"}, {"agent", "apply", "desired.json"}, {"rollback", "--service", "app", "--destination", "production"}} {
		var stderr bytes.Buffer
		args = append(args, "--state-dir", t.TempDir())
		if code := cli.New("test").Run(context.Background(), args, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "serve agent run") {
			t.Fatalf("expected migration error: %d %s", code, &stderr)
		}
	}
}
func TestExecPreservesOutputOnFailureThroughAgent(t *testing.T) {
	rt := fake.NewRuntime()
	id := createManagedContainer(t, rt, "app", "web")
	if err := rt.StartContainer(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	rt.SetExecResult("app", "partial output\n", io.ErrUnexpectedEOF)
	cmd := socketCommand(t, rt, "")
	var out, errOut bytes.Buffer
	code := cmd.Run(context.Background(), []string{"exec", "--container", "app", "--", "echo", "--socket", "literal"}, &out, &errOut)
	if code != 1 || out.String() != "partial output\n" || !strings.Contains(errOut.String(), "unexpected EOF") {
		t.Fatalf("exec: %d %q %q", code, &out, &errOut)
	}
}
