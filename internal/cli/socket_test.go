package cli_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptimenine/serve/internal/agent/daemon"
	"github.com/uptimenine/serve/internal/cli"
	"github.com/uptimenine/serve/internal/runtime/fake"
)

// Exercise the real CLI, Unix transport and agent. Only Docker is replaced.
type agentCommand struct {
	command *cli.Command
	socket  string
}

func (c agentCommand) Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	args = append([]string(nil), args...)
	at := len(args)
	for i, arg := range args {
		if arg == "--" {
			at = i
			break
		}
	}
	args = append(args[:at], append([]string{"--socket", c.socket}, args[at:]...)...)
	return c.command.Run(ctx, args, out, errOut)
}
func socketCommand(t *testing.T, rt *fake.Runtime, stateDir string, configure ...func(*daemon.Config)) agentCommand {
	t.Helper()
	dir, err := os.MkdirTemp("", "servecli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	if stateDir == "" {
		stateDir = filepath.Join(dir, "state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := daemon.Config{Runtime: rt, StateDir: stateDir, SocketPath: socket, ReconcileInterval: time.Hour, ErrorLog: io.Discard}
	for _, apply := range configure {
		apply(&cfg)
	}
	d := daemon.New(cfg)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("agent did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	// Supplying an unrelated runtime catches accidental CLI-side mutations.
	return agentCommand{command: cli.New("test", cli.WithRuntime(fake.NewRuntime())), socket: socket}
}

func TestLocalCommandsRequireAgentWithoutDockerFallback(t *testing.T) {
	dir := t.TempDir()
	desired := writeDesiredState(t, dir, desiredState("v1"))
	config := filepath.Join(dir, "serve.yml")
	if err := os.WriteFile(config, []byte("service: my-app\nimage: busybox\nservers:\n  web:\n    hosts: [localhost]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"status"}, {"logs", "--container", "app"}, {"events", "--once"}, {"doctor"},
		{"remove", "--force"}, {"prune", "--force"}, {"rollback", "--service", "my-app", "--destination", "production"},
		{"exec", "--container", "app", "--", "echo", "--socket", "literal"},
		{"agent", "apply", desired}, {"deploy", "--local", "--config", config},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rt := fake.NewRuntime()
			socket := filepath.Join(dir, "missing.sock")
			c := agentCommand{command: cli.New("test", cli.WithRuntime(rt)), socket: socket}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var stderr bytes.Buffer
			if code := c.Run(ctx, args, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), socket) {
				t.Fatalf("expected socket error, got code=%d: %s", code, &stderr)
			}
			if len(rt.Operations()) != 0 {
				t.Fatalf("CLI touched Docker: %v", rt.Operations())
			}
		})
	}
}
