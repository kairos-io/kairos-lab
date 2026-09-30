// The _linux_test.go suffix gives this file the same build constraint as the
// code it tests: fakeHost, cleanupNMConnections and CleanupStaleNetworkResources
// exist only in the Linux build of the package.
package vm

import (
	"io/fs"
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
	// Said out loud rather than left to the fake's default, which answers
	// "is this a tun/tap device" from whether the name parses as a generated
	// one. That default is the name-equals-device conflation this guard is
	// about, so a test that leaned on it would pass for the wrong reason and
	// would keep passing if the guard started asking the name again.
	h.realTapDevices[DefaultTapName] = true

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

// TestStaleCleanupRefusesWhenTheDeviceStatCannotAnswer covers the third of the
// three answers refuseForeignStoredDevice gives, and the only one neither
// test above reaches: /sys/class/net/<name> is there but the stat fails with
// something other than "no such file or directory", so the host cannot say
// what kind of device this is.
//
// It is a separate case from both others and not a shade of either. "No such
// device" passes, because a leftover profile with no device is what the stale
// cleanup exists for; "wrong kind of device" refuses, because that is the
// bug. An unanswerable stat looks like the first to every probe on this path
// -- isLinuxBridge is `os.Stat(...); return err == nil` and cannot tell them
// apart -- which is exactly why netDeviceExists reports the error instead of
// a bool, and why a regression here would be silent: the name would sail
// through the guard into `sudo ip link delete`.
func TestStaleCleanupRefusesWhenTheDeviceStatCannotAnswer(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true
	h.invisibleBridges[DefaultBridgeName] = fs.ErrPermission

	err := CleanupStaleNetworkResources(storedState(DefaultBridgeName))
	if err == nil {
		t.Fatal("the stale cleanup accepted a bridge name whose device stat failed, want a refusal")
	}
	if !strings.Contains(err.Error(), DefaultBridgeName) {
		t.Errorf("the refusal does not name the interface it refused: %v", err)
	}
	for _, argv := range h.commands {
		t.Errorf("the refusal still issued a command: %v", argv)
	}
}

// TestCleanupRefusesWhenTheTapStatCannotAnswer is the same case on the second
// name the choke point promises to guard. The bridge name is a real
// kairos-lab bridge here, so the only thing that can refuse is the tap.
func TestCleanupRefusesWhenTheTapStatCannotAnswer(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true
	h.links[DefaultBridgeName] = true
	h.bridges[DefaultBridgeName] = true
	h.invisibleBridges[DefaultTapName] = fs.ErrPermission

	err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, "", false)
	if err == nil {
		t.Fatal("the teardown accepted a tap name whose device stat failed, want a refusal")
	}
	if !strings.Contains(err.Error(), DefaultTapName) {
		t.Errorf("the refusal does not name the interface it refused: %v", err)
	}
	for _, argv := range h.commands {
		t.Errorf("the refusal still issued a command: %v", argv)
	}
}

// TestPreflightRefusesAHostNICInEveryMode is the half of #5051 that the choke
// point alone does not close.
//
// linuxNetworkPreflight reaches cleanupNMConnections through the stale-resource
// branch, and used to read everything it returned as "a teardown ran and part
// of it failed". A refusal is the opposite of that -- nothing ran -- and the
// two modes got it wrong in two different ways: bridged DROPPED the refusal
// and carried on into `nmcli connection modify <name>` over the name just
// refused, and shared reported it in words describing a partial teardown.
//
// Both modes must stop, so both are driven here. The assertion that no
// command was issued is the one that fails against the old bridged path.
func TestPreflightRefusesAHostNICInEveryMode(t *testing.T) {
	for _, mode := range []string{"bridged", "shared"} {
		t.Run(mode, func(t *testing.T) {
			h := newFakeHost(t)
			seedHostNIC(h)

			st := storedState(hostNIC)
			st.Network.Mode = mode
			err := linuxNetworkPreflight(st, t.TempDir(), mode, hostNIC, DefaultTapName, TapConnNameForIndex(hostNIC, 0), false)
			if err == nil {
				t.Fatal("the preflight started over a stored bridge name that is a physical NIC, want a refusal")
			}
			if !strings.Contains(err.Error(), hostNIC) {
				t.Errorf("the refusal does not name the interface it refused: %v", err)
			}
			// The refusal fired before anything was issued, so the message
			// must not tell the user their host has been changed.
			if strings.Contains(err.Error(), "the host's networking has changed") {
				t.Errorf("the refusal describes itself as a partial teardown: %v", err)
			}
			for _, argv := range h.commands {
				t.Errorf("the preflight still issued a command over the refused name: %v", argv)
			}
		})
	}
}
