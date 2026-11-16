package volume_test

import (
	"strings"
	"testing"

	"github.com/uptimenine/serve/internal/volume"
)

func TestValidateAllRejectsReservedServeVolumes(t *testing.T) {
	for _, raw := range []string{"serve-proxy-data:/data", "serve-anything:/data:ro"} {
		err := volume.ValidateAll([]string{raw})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("ValidateAll(%q) = %v, want reserved-name error", raw, err)
		}
	}
	if err := volume.ValidateAll([]string{"app-data:/data", "/srv/serve-config:/config:ro"}); err != nil {
		t.Fatalf("ordinary volumes rejected: %v", err)
	}
}
