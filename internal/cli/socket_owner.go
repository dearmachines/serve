package cli

import (
	"fmt"
	"os"
	"syscall"
)

// verifyAgentSocket refuses to talk to a socket that neither root nor the
// caller owns. The CLI sends desired state (secret references, images) to
// the agent, so a socket squatted by another local user in a shared
// directory must not receive it.
func verifyAgentSocket(path string) error {
	return verifySocketOwner(path, map[uint32]bool{0: true, uint32(os.Getuid()): true})
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
