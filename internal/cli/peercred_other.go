//go:build !linux && !darwin

package cli

import "errors"

func peerUID(uintptr) (uint32, error) {
	return 0, errors.New("peer credentials are not supported on this platform")
}
