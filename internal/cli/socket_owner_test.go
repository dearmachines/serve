package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentSocketMustBeOwnedByRootOrTheCaller(t *testing.T) {
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	self := uint32(os.Getuid())
	if err := verifySocketOwner(path, map[uint32]bool{self: true}); err != nil {
		t.Fatalf("own socket rejected: %v", err)
	}
	// Model a socket squatted by another local account.
	err = verifySocketOwner(path, map[uint32]bool{self + 1: true})
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("socket owned by another user accepted: %v", err)
	}

	regular := filepath.Join(dir, "file")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySocketOwner(regular, map[uint32]bool{self: true}); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("non-socket accepted: %v", err)
	}
}

func TestAgentConnectionIsVerifiedByPeerCredentials(t *testing.T) {
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	self := uint32(os.Getuid())

	conn, err := dialVerifiedAgent(context.Background(), path, map[uint32]bool{self: true})
	if err != nil {
		t.Fatalf("own agent rejected: %v", err)
	}
	conn.Close()

	if _, err := dialVerifiedAgent(context.Background(), path, map[uint32]bool{self + 1: true}); err == nil {
		t.Fatal("agent run by an untrusted uid accepted")
	}

	// The process behind the socket, not the path checked before dialing,
	// is what the final check trusts.
	raw, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	uid, err := connPeerUID(raw)
	if err != nil || uid != self {
		t.Fatalf("peer uid = %d %v, want %d", uid, err, self)
	}
}
