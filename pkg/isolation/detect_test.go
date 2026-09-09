package isolation

import (
	"context"
	"runtime"
	"testing"

	"jdix.io/sandbox/pkg/bwrap"
)

func TestDetectNeverErrorsAndAlwaysNamesATier(t *testing.T) {
	res := Detect(context.Background(), "")
	if res.Tier == "" {
		t.Fatal("Detect must always name a tier")
	}
	if res.Reason == "" {
		t.Fatal("Detect must always explain itself; the reason is shown in the Console health page")
	}
	if res.Tier.Rank() == 0 {
		t.Fatalf("Detect returned an unknown tier %q", res.Tier)
	}
	if runtime.GOOS != "linux" && res.Tier != bwrap.TierChroot {
		t.Fatalf("non-Linux must report the chroot tier, got %q", res.Tier)
	}
}

func TestProbeArgsNeverIsolateTheNetwork(t *testing.T) {
	for _, args := range [][]string{probeArgsUserns(), probeArgsCapAdmin()} {
		for _, a := range args {
			if a == "--unshare-net" {
				t.Fatal("probes must not use --unshare-net; it would not reflect how sandboxes actually run")
			}
		}
	}
}
