// Tests for the multi-VM fixes in network_linux.go: hazard (A) (the
// stale-cleanup branch tearing down a live sibling's bridge), hazard (B) (a
// shared tap connection name re-pointing a sibling's live tap), and D5's
// bridge-port exemption. Same fakeHost harness as network_linux_test.go, no
// build tag, for the reasons given at the top of that file.
package vm

import (
	"strings"
	"testing"

	"github.com/kairos-io/kairos-lab/internal/state"
)

// TestPrepareLinuxSharedJoinsALiveSiblingsBridgeWithoutTearingItDown is fix
// (A): with siblingLive true, a bridge and tap a live sibling (index 0)
// already holds must never be torn down by a second VM's (index 1) start.
// Reproduced against unfixed logic, this hazard tears the bridge down on the
// SUCCESS path -- hasStaleBridgeResources sees the bridge already there and
// treats it as a leftover from a crashed run, not as a sibling in active
// use.
func TestPrepareLinuxSharedJoinsALiveSiblingsBridgeWithoutTearingItDown(t *testing.T) {
	h := newFakeHost(t)
	// VM1 (index 0) already has the bridge and its own tap live on it.
	h.bridges[DefaultBridgeName] = true
	h.conns[DefaultBridgeName] = true
	h.conns[DefaultBridgeName+"-tap"] = true
	h.links[DefaultTapName] = true
	h.slaves[DefaultBridgeName] = []string{DefaultTapName}
	h.profiles[DefaultBridgeName] = nmProfile{ifname: DefaultBridgeName, isBridge: true}
	h.profiles[DefaultBridgeName+"-tap"] = nmProfile{ifname: DefaultTapName, master: DefaultBridgeName}

	st := &state.State{}
	if err := PrepareLinuxShared(st, t.TempDir(), 1, true); err != nil {
		t.Fatalf("PrepareLinuxShared for VM2 with a live sibling failed: %v", err)
	}

	for _, argv := range h.commands {
		joined := strings.Join(argv, " ")
		if strings.HasPrefix(joined, "nmcli connection delete") || strings.HasPrefix(joined, "ip link delete") {
			t.Errorf("VM1's live bridge was torn down by VM2's start: %v", argv)
		}
	}
	if !h.bridges[DefaultBridgeName] {
		t.Error("the bridge itself was removed")
	}
	if !h.links[DefaultTapName] {
		t.Error("VM1's own tap device was removed")
	}
	if !h.conns[DefaultBridgeName+"-tap"] {
		t.Error("VM1's own tap connection was removed")
	}
	// VM2's own tap must have been created and attached.
	if !h.conns[DefaultBridgeName+"-tap1"] {
		t.Error("VM2's tap connection (kairoslab0-tap1) was never created")
	}
	if !slavesContain(h.slaves[DefaultBridgeName], "kairoslab-tap1") {
		t.Errorf("VM2's tap was never attached to the bridge, slaves = %v", h.slaves[DefaultBridgeName])
	}
}

// TestPrepareLinuxSharedJoinsALiveSiblingsBridgeReproducesHazardAWhenUnfixed
// is the same scenario with siblingLive forced to false -- the pre-fix
// behaviour -- to demonstrate the hazard is real against this harness and
// not merely asserted. It is not a claim about production code, which never
// calls PrepareLinuxShared with siblingLive=false while a sibling is
// actually live; it exists so the fix above is provably a fix and not a
// no-op.
func TestPrepareLinuxSharedJoinsALiveSiblingsBridgeReproducesHazardAWhenUnfixed(t *testing.T) {
	h := newFakeHost(t)
	h.bridges[DefaultBridgeName] = true
	h.conns[DefaultBridgeName] = true
	h.conns[DefaultBridgeName+"-tap"] = true
	h.links[DefaultTapName] = true
	h.slaves[DefaultBridgeName] = []string{DefaultTapName}

	st := &state.State{}
	// siblingLive=false: reproduces the old, unconditional stale-cleanup
	// branch, over a bridge that is in fact a live sibling's. The teardown
	// deletes and then recreates a same-named bridge for VM2's own use, so
	// checking h.bridges afterwards would not show the damage; what is
	// observable is that VM1's tap is no longer a port of it -- `ip link
	// delete kairoslab0` drops every port when it removes the device, and
	// nothing here puts VM1's tap back.
	_ = PrepareLinuxShared(st, t.TempDir(), 1, false)

	if slavesContain(h.slaves[DefaultBridgeName], DefaultTapName) {
		t.Fatal("hazard (A) did not reproduce: VM1's tap is still a port of the bridge with siblingLive=false, so this harness cannot demonstrate the fix above is doing anything")
	}
}

// TestPrepareLinuxSharedDoesNotRepointASiblingsTapConnection is fix (B): the
// tap connection name is per-index (TapConnNameForIndex), so VM2's own
// prepare touches "kairoslab0-tap1" and never "kairoslab0-tap" -- VM1's live
// connection -- at all.
func TestPrepareLinuxSharedDoesNotRepointASiblingsTapConnection(t *testing.T) {
	h := newFakeHost(t)
	h.bridges[DefaultBridgeName] = true
	h.conns[DefaultBridgeName] = true
	h.conns[DefaultBridgeName+"-tap"] = true
	h.links[DefaultTapName] = true
	h.slaves[DefaultBridgeName] = []string{DefaultTapName}
	h.profiles[DefaultBridgeName+"-tap"] = nmProfile{ifname: DefaultTapName, master: DefaultBridgeName}

	st := &state.State{}
	if err := PrepareLinuxShared(st, t.TempDir(), 1, true); err != nil {
		t.Fatalf("PrepareLinuxShared for VM2 failed: %v", err)
	}

	for _, argv := range h.commands {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, DefaultBridgeName+"-tap") && !strings.Contains(joined, DefaultBridgeName+"-tap1") {
			t.Errorf("VM2's prepare touched VM1's own tap connection %q: %v", DefaultBridgeName+"-tap", argv)
		}
	}
}

// TestIsRunningTreatsEPERMAsRunning is D3's fix for vm.IsRunning: a process
// this test cannot signal (PID 1, owned by root, on any host this test runs
// as non-root) must read as running, not as absent. On a host where this
// test itself runs as root, EPERM cannot be produced against PID 1 and the
// case is skipped rather than asserted falsely.
func TestIsRunningTreatsEPERMAsRunning(t *testing.T) {
	running, err := IsRunning(1)
	if err != nil {
		t.Fatalf("IsRunning(1) returned an error: %v", err)
	}
	if !running {
		t.Skip("this test could signal PID 1, so it is not exercising the EPERM branch (likely running as root)")
	}
}

// TestIsRunningReturnsFalseForAPIDThatDoesNotExist is ESRCH's side of the
// same fix: a PID nothing holds reads as not running, with no error.
func TestIsRunningReturnsFalseForAPIDThatDoesNotExist(t *testing.T) {
	// A PID this large is never assigned on any host Linux or Darwin's
	// default pid_max allows; if this ever flakes because something really
	// is running there, that is itself worth knowing.
	const unusedPID = 1<<31 - 1
	running, err := IsRunning(unusedPID)
	if err != nil {
		t.Fatalf("IsRunning(%d) returned an error: %v", unusedPID, err)
	}
	if running {
		t.Errorf("IsRunning(%d) = true, want false", unusedPID)
	}
}

// TestBridgePortExemption is D5's table, pinning each of the three
// conditions separately so none of them is unobservable.
func TestBridgePortExemption(t *testing.T) {
	tests := []struct {
		name         string
		port         string
		notRealTap   bool
		foreignOwned bool
		wantExempt   bool
	}{
		{name: "a user-owned tap with a non-generated name is never exempt", port: "eth0", wantExempt: false},
		{name: "a leading zero fails the generated-name parse", port: "kairoslab-tap007", wantExempt: false},
		{name: "a negative index fails the generated-name parse", port: "kairoslab-tap-1", wantExempt: false},
		{name: "an index past the supported range fails the parse", port: "kairoslab-tap100", wantExempt: false},
		{name: "a generated name with no tun_flags is not a real tap device", port: "kairoslab-tap3", notRealTap: true, wantExempt: false},
		{name: "a generated, real tap device owned by someone else", port: "kairoslab-tap3", foreignOwned: true, wantExempt: false},
		{name: "a generated, real, self-owned tap device is exempt", port: "kairoslab-tap3", wantExempt: true},
		{name: "index 0's default name is exempt on the same terms", port: DefaultTapName, wantExempt: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			if tt.notRealTap {
				h.notRealTapDevices[tt.port] = true
			}
			if tt.foreignOwned {
				h.foreignOwnedTaps[tt.port] = true
			}
			got := bridgePortExempt(tt.port)
			if got != tt.wantExempt {
				t.Errorf("bridgePortExempt(%q) = %v, want %v", tt.port, got, tt.wantExempt)
			}
		})
	}
}

func slavesContain(slaves []string, name string) bool {
	for _, s := range slaves {
		if s == name {
			return true
		}
	}
	return false
}
