package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
)

// verifyAgentSocket refuses to talk to a socket that neither root nor the
// caller owns. The CLI sends desired state (secret references, images) to
// the agent, so a socket squatted by another local user in a shared
// directory must not receive it.
func trustedAgentUIDs() map[uint32]bool {
	return map[uint32]bool{0: true, uint32(os.Getuid()): true}
}

// dialVerifiedAgent connects to the agent socket and accepts the connection
// only if the process on the other end runs as a trusted uid. Checking the
// peer, not just the path beforehand, closes the window in which the socket
// could be swapped between the check and the connect.
func dialVerifiedAgent(ctx context.Context, path string, trusted map[uint32]bool) (net.Conn, error) {
	if err := verifySocketOwner(path, trusted); err != nil {
		return nil, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	uid, err := connPeerUID(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("agent socket %s: read peer credentials: %w", path, err)
	}
	if !trusted[uid] {
		conn.Close()
		return nil, fmt.Errorf("agent socket %s: peer process runs as uid %d, not root or the current user; refusing to connect", path, uid)
	}
	return conn, nil
}

func connPeerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix socket connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var credErr error
	if err := raw.Control(func(fd uintptr) { uid, credErr = peerUID(fd) }); err != nil {
		return 0, err
	}
	return uid, credErr
}

func verifySocketOwner(path string, trusted map[uint32]bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("agent socket %s is not a socket", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("agent socket %s: cannot determine owner", path)
	}
	if !trusted[stat.Uid] {
		return fmt.Errorf("agent socket %s is owned by uid %d, not root or the current user; refusing to connect", path, stat.Uid)
	}
	return nil
}
