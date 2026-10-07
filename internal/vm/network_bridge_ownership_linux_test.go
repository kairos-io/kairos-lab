// The _linux_test.go suffix gives this file the same build constraint as the
// code it tests: fakeHost and the teardown entry points exist only in the
// Linux build of the package.
package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-lab/internal/state"
)

// dockerBridge is the issue's reproduction, and the reason the kind check is
// not enough on its own: it passes validateStoredInterfaceName, it is a real
// Linux bridge, and it is on any host that runs Docker.
const dockerBridge = "docker0"

// TestCleanupRefusesABridgeTheHostAlreadyHad is kairos-io/kairos#5288's
// reproduction. A stored bridge name of "docker0" must not turn into
// `sudo nmcli connection delete docker0` and `sudo ip link delete docker0`,
// which takes every container on that bridge off the network.
func TestCleanupRefusesABridgeTheHostAlreadyHad(t *testing.T) {
	for _, foreign := range []string{dockerBridge, "virbr0", "br0"} {
		t.Run(foreign, func(t *testing.T) {
			h := newFakeHost(t)
			// A real bridge, with the profile a NetworkManager-managed one
			// would have, and no kairos-lab mark on either: seeded without
			// seedOurBridge, which is the difference this guard reads.
			h.conns[foreign] = true
			h.links[foreign] = true
			h.bridges[foreign] = true

			err := CleanupStaleNetworkResources(storedState(foreign))
			if err == nil {
				t.Fatalf("the stale cleanup accepted %q, a bridge kairos-lab did not create", foreign)
			}
			if !strings.Contains(err.Error(), foreign) {
				t.Errorf("the refusal does not name the bridge it refused: %v", err)
			}
			for _, argv := range h.commands {
				t.Errorf("the refusal still issued a command: %v", argv)
			}
		})
	}
}

// TestCleanupTearsDownTheBridgeItsOwnStartMarked is the other half, end to
// end and with nothing seeded: the start builds the bridge and marks it, and
// the teardown that follows accepts it on the strength of that mark alone.
//
// Written this way deliberately. Every other test here seeds the mark, so
// each one would still pass if markBridgeAsOurs stopped being called; this
// one is the single place where the mark has to be put on by the production
// path for the assertion to hold.
func TestCleanupTearsDownTheBridgeItsOwnStartMarked(t *testing.T) {
	h := newFakeHost(t)
	st := &state.State{}

	if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxShared: %v", err)
	}
	if h.aliases[DefaultBridgeName] != bridgeOwnerAlias {
		t.Errorf("the start left no device alias on its bridge: %q", h.aliases[DefaultBridgeName])
	}
	if got := h.userData[DefaultBridgeName][bridgeOwnerMarkKey]; got != bridgeOwnerMarkValue {
		t.Errorf("the start left no user data on its bridge profile: %q", got)
	}

	before := len(h.commands)
	if err := CleanupStaleNetworkResources(st); err != nil {
		t.Fatalf("the teardown refused the bridge this run had just marked: %v", err)
	}
	if len(h.commands) == before {
		t.Fatal("the teardown issued no command over the bridge this run created")
	}
}

// TestCleanupAcceptsEitherMarkOnItsOwn pins why there are two marks. Each one
// survives a thing the other does not, so the teardown has to accept a bridge
// carrying only one of them.
func TestCleanupAcceptsEitherMarkOnItsOwn(t *testing.T) {
	tests := []struct {
		name string
		seed func(*fakeHost)
	}{
		{
			// A reboot: the bridge link is rebuilt by NetworkManager from the
			// keyfile, so the alias is gone and the user data is not. This is
			// an ordinary `cleanup` after a bridged run, whose profile
			// carries connection.autoconnect yes.
			name: "the user data alone, after a reboot",
			seed: func(h *fakeHost) {
				h.userData[DefaultBridgeName] = map[string]string{bridgeOwnerMarkKey: bridgeOwnerMarkValue}
			},
		},
		{
			// A half-cleaned host: the profile is gone and the device it
			// built is still up, which is the other direction of the same
			// interruption the stale cleanup exists for.
			name: "the device alias alone, with the profile deleted",
			seed: func(h *fakeHost) {
				h.aliases[DefaultBridgeName] = bridgeOwnerAlias
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.conns[DefaultBridgeName] = true
			h.links[DefaultBridgeName] = true
			h.bridges[DefaultBridgeName] = true
			tt.seed(h)

			if err := CleanupStaleNetworkResources(storedState(DefaultBridgeName)); err != nil {
				t.Fatalf("the teardown refused a bridge carrying a kairos-lab mark: %v", err)
			}
			if len(h.commands) == 0 {
				t.Fatal("the teardown issued no command over its own bridge")
			}
		})
	}
}

// TestBridgeRefusalNamesTheWayOut: a bridge an older kairos-lab built carries
// no mark and is refused with every other unmarked bridge, so the message has
// to say what to do about it rather than only that it will not proceed.
func TestBridgeRefusalNamesTheWayOut(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true
	h.links[DefaultBridgeName] = true
	h.bridges[DefaultBridgeName] = true

	err := CleanupStaleNetworkResources(storedState(DefaultBridgeName))
	if err == nil {
		t.Fatal("an unmarked bridge was accepted, want a refusal")
	}
	for _, want := range []string{bridgeOwnerAlias, bridgeOwnerMarkKey, "kairos-lab start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	_ = h
}

// TestAStartFailingToMarkIsAFailedStart. The alternative is a VM running on a
// bridge no later teardown will agree to remove, and nothing tells the user
// that until they try to clean up.
func TestAStartFailingToMarkIsAFailedStart(t *testing.T) {
	for _, failing := range []string{
		"ip link set " + DefaultBridgeName + " alias " + bridgeOwnerAlias,
		"nmcli connection modify " + DefaultBridgeName + " +user.data " + bridgeOwnerMarkKey + "=" + bridgeOwnerMarkValue,
	} {
		t.Run(failing, func(t *testing.T) {
			h := newFakeHost(t)
			h.failCmd = func(argv []string) error {
				if strings.Join(argv, " ") == failing {
					return fmt.Errorf("exit status 1")
				}
				return nil
			}

			st := &state.State{}
			if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err == nil {
				t.Fatal("the start succeeded although the bridge could not be marked")
			}
			if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
				t.Errorf("state was written for a run that failed: %+v", st.Network)
			}
		})
	}
}

// TestUserDataHasItem covers the parse of what nmcli prints, which is the one
// piece of this guard that is a string format rather than a file on the host.
func TestUserDataHasItem(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"the spacing nmcli uses today", bridgeOwnerMarkKey + " = " + bridgeOwnerMarkValue, true},
		{"no spacing", bridgeOwnerMarkKey + "=" + bridgeOwnerMarkValue, true},
		{"one item among several", "org.freedesktop.a = b, " + bridgeOwnerMarkKey + " = " + bridgeOwnerMarkValue + ", c.d = e", true},
		{"an empty dictionary", "", false},
		{"another application's data", "org.freedesktop.a = b", false},
		{"our key with another value", bridgeOwnerMarkKey + " = 0", false},
		{"our value under another key", "other.key = " + bridgeOwnerMarkValue, false},
		{"the key as a substring of a longer one", "x" + bridgeOwnerMarkKey + " = " + bridgeOwnerMarkValue, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userDataHasItem(tt.out, bridgeOwnerMarkKey, bridgeOwnerMarkValue); got != tt.want {
				t.Errorf("userDataHasItem(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}
