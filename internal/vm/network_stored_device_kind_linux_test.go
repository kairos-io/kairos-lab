// The _linux_test.go suffix gives this file the same build constraint as the
// code it tests: fakeHost, cleanupNMConnections and CleanupStaleNetworkResources
// exist only in the Linux build of the package.
package vm

import (
	"strings"
	"testing"

	"github.com/kairos-io/kairos-lab/internal/state"
)

// hostNIC is a name that passes validateStoredInterfaceName -- letters and
// digits, 4 bytes, no leading dash -- and that names a real device on almost
// every Linux host. That combination is the bug: the charset rule has no
// opinion about what the name refers to.
const hostNIC = "eth0"

// seedHostNIC gives the fake host a physical NIC and the NetworkManager
// profile named after it, which is how distributions that name the profile
// after the device look. It is deliberately NOT a bridge.
func seedHostNIC(h *fakeHost) {
	h.conns[hostNIC] = true
	h.links[hostNIC] = true
}

// storedState is a state.json naming `name` as the bridge, in a mode that
// resolves the stored name rather than replacing it.
func storedState(name string) *state.State {
	st := &state.State{}
	st.Network.Mode = "shared"
	st.Network.BridgeName = name
	return st
}

// TestStaleCleanupRefusesAHostNICAsTheBridge is the issue's reproduction:
// a stored bridge name of "eth0" must not turn into
// `sudo nmcli connection delete eth0` and `sudo ip link delete eth0`.
func TestStaleCleanupRefusesAHostNICAsTheBridge(t *testing.T) {
	h := newFakeHost(t)
	seedHostNIC(h)

	err := CleanupStaleNetworkResources(storedState(hostNIC))
	if err == nil {
		t.Fatal("the stale cleanup accepted a stored bridge name that is a physical NIC, want a refusal")
	}
	if !strings.Contains(err.Error(), hostNIC) {
		t.Errorf("the refusal does not name the interface it refused: %v", err)
	}

	// The refusal has to come before any command is issued, not be a
	// summary of failures after the fact.
	for _, argv := range h.commands {
		t.Errorf("the refusal still issued a command: %v", argv)
	}
}

// TestStaleCleanupStillTearsDownItsOwnBridge pins the case the refusal must
// not touch: a real kairos-lab bridge, which is what this path exists for.
func TestStaleCleanupStillTearsDownItsOwnBridge(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true
	h.links[DefaultBridgeName] = true
	h.bridges[DefaultBridgeName] = true

	if err := CleanupStaleNetworkResources(storedState(DefaultBridgeName)); err != nil {
		t.Fatalf("the stale cleanup refused its own bridge: %v", err)
	}
	if len(h.commands) == 0 {
		t.Fatal("the stale cleanup issued no command over its own bridge")
	}
}

// TestStaleCleanupStillRunsWhenTheBridgeIsAlreadyGone pins the other case the
// refusal must not swallow: the device is gone but the NetworkManager profile
// it left behind is not, which is the ordinary "interrupted setup" this entry
// point is named for. A name that resolves to no device at all is not evidence
// of a foreign device.
func TestStaleCleanupStillRunsWhenTheBridgeIsAlreadyGone(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true

	if err := CleanupStaleNetworkResources(storedState(DefaultBridgeName)); err != nil {
		t.Fatalf("the stale cleanup refused a bridge that is already gone: %v", err)
	}
	if len(h.commands) == 0 {
		t.Fatal("the stale cleanup issued no command over the leftover profile")
	}
}

// TestCleanupRefusesAHostNICAsTheTap covers the second name the choke point
// feeds to `ip link delete`. No caller reaches cleanupNMConnections with a tap
// name read fresh from state.json today -- StaleNetworkResourceNames and
// CleanupLinuxBridge both pass TapNameForIndex's generated output, and the
// stored st.Network.TapName goes to qemu's -netdev instead -- so this is the
// guard holding the argument rather than the caller. It is checked here
// because the choke point promises it for both names, and the callers that
// make that promise true are free to change.
func TestCleanupRefusesAHostNICAsTheTap(t *testing.T) {
	h := newFakeHost(t)
	seedHostNIC(h)
	h.conns[DefaultBridgeName] = true
	h.links[DefaultBridgeName] = true
	h.bridges[DefaultBridgeName] = true

	err := cleanupNMConnections(DefaultBridgeName, hostNIC, "", false)
	if err == nil {
		t.Fatal("the teardown accepted a tap name that is a physical NIC, want a refusal")
	}
	if !strings.Contains(err.Error(), hostNIC) {
		t.Errorf("the refusal does not name the interface it refused: %v", err)
	}
	for _, argv := range h.commands {
		t.Errorf("the refusal still issued a command: %v", argv)
	}
}

// TestCleanupStillDeletesItsOwnTap pins that the tap guard does not refuse the
// ordinary case: a real tun/tap device kairos-lab created.
func TestCleanupStillDeletesItsOwnTap(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true
	h.links[DefaultBridgeName] = true
	h.bridges[DefaultBridgeName] = true
	h.links[DefaultTapName] = true

	if err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, "", false); err != nil {
		t.Fatalf("the teardown refused its own tap: %v", err)
	}
	var deletedTap bool
	for _, argv := range h.commands {
		if len(argv) >= 4 && argv[0] == "ip" && argv[1] == "link" && argv[2] == "delete" && argv[3] == DefaultTapName {
			deletedTap = true
		}
	}
	if !deletedTap {
		t.Errorf("the teardown never deleted its own tap; commands: %v", h.commands)
	}
}
