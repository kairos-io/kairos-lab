package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kairos-io/kairos-lab/internal/state"
)

const (
	DefaultBridgeName = "kairoslab0"
	DefaultTapName    = "kairoslab-tap0"
)

// resolveLinuxBridgeName resolves and validates the host-level bridge name --
// managedBridgeName's answer for this state. It is resolved once, before
// either PrepareLinuxBridge or PrepareLinuxShared can compute this VM's own
// tap connection name (TapConnNameForIndex takes the bridge as an argument).
//
// What is validated is what is STORED, not what managedBridgeName goes on to
// resolve. A malformed name is a malformed name whatever mode recorded it, and
// the two questions are separate: whether a value is safe to hand to root, and
// whether this run is the one that owns it. Ask only the second and a name
// this run ignores stays in the file unexamined until the mode that does use
// it reads it back. An empty field is "nothing was recorded" rather than a
// name of no length; it resolves to a compiled-in default that needs no
// checking, and the validator would reject it.
//
// The stored name is about to be interpolated into root-run nmcli and ip
// commands, into a filesystem path and into the cleanup plan the user is asked
// to confirm. A rejected name is an error and not a quiet fall back to the
// default: a value someone put in state.json that silently does nothing is its
// own surprise.
func resolveLinuxBridgeName(st *state.State) (string, error) {
	if st.Network.BridgeName != "" {
		if err := validateStoredInterfaceName("bridge name", st.Network.BridgeName); err != nil {
			return "", err
		}
	}
	return managedBridgeName(st.Network), nil
}

// linuxNetworkPreflight performs the host-side checks PrepareLinuxBridge and
// PrepareLinuxShared both make, once bridge and tapConn -- this VM's own,
// already resolved from its index -- are known. mode is the user-facing
// network mode ("bridged", "shared"); it appears in the NetworkManager error
// so the message names the mode the caller actually asked for.
//
// siblingLive guards against a second VM's start tearing down a live
// sibling's bridge: when true, a live sibling already holds this bridge and
// the stale-cleanup branch below is skipped entirely, so this start joins the
// bridge instead of tearing down what the sibling is using. When false -- no
// other VM in this config dir is live -- today's teardown runs unchanged, so
// a single-VM host still recovers from a genuinely stale bridge exactly as
// before this change.
func linuxNetworkPreflight(st *state.State, runtimeDir, mode, bridge, tapDevice, tapConn string, siblingLive bool) error {
	if !networkManagerActive() {
		return fmt.Errorf("NetworkManager is required for %s networking on Linux. Please install and enable NetworkManager, or use --network user for port-forwarded access", mode)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}

	// Check for stale resources from a previous run. The predicate is shared
	// with HasStaleNetworkResources so the two cannot disagree about what
	// stale means; see hasStaleBridgeResources for why every term of it
	// matters to the shared path too. It is skipped outright when a sibling
	// is live: see siblingLive above.
	if !siblingLive && hasStaleBridgeResources(bridge, tapConn) {
		fmt.Println("Found stale network configuration, cleaning up...")
		// false: this branch only runs when no sibling is live, so the full,
		// unrestricted teardown is exactly what today's single-VM host gets.
		cleanupErr := cleanupNMConnections(bridge, tapDevice, tapConn, false)
		// A leftover that will not go is fatal for shared and survivable for
		// bridged. Every connection this teardown deletes is one the bridged
		// path recreates and re-modifies a few lines later, with the master,
		// the slave type and the autoconnect that run wants, so a failed
		// delete leaves it with a profile it is about to overwrite -- which
		// is why the error is dropped there. The shared path rewrites none
		// of them: its bridge carries ipv4.method shared and its only port
		// is meant to be the tap, and a surviving profile that carries
		// `master <bridge> slave-type bridge` has NetworkManager enslave a
		// host NIC to that bridge when it comes up.
		//
		// What this refusal knows, though, is only that a teardown step
		// failed -- not that such a profile survived. The step may have been
		// the `nmcli device connect` reconnect at the end, where every
		// delete succeeded and nothing is enslaved to anything. Not knowing
		// the state of the host is reason enough to stop a mode whose whole
		// promise is that no host interface is attached, so the refusal
		// stays; the message below claims no more than that. The invariant
		// itself is enforced where it is observable, by the bridge-port
		// assertion in prepareLinuxSharedWithNM, which reads the kernel's
		// port list even on a host this preflight found clean.
		//
		// This is not a refusal that comes before anything was touched,
		// either. cleanupNMConnections stops at no failure, so every later
		// step was reached -- which is not the same as every later step
		// having run: the two `ip link delete`s are skipped when linkExists
		// cannot see their interface, and the reconnect when findBridgeSlave
		// named nobody or when the connection that reconnect would replace
		// was not deleted. Which of them issued anything is not knowable from
		// the joined error, so the message names the gates instead of
		// listing the steps as though they had all run.
		// A refusal is not a failed teardown. cleanupNMConnections refuses a
		// name before it issues anything, so nothing was touched and no later
		// step of this start makes the stored name right. Both modes must
		// stop on it: shared would otherwise report it in the words of the
		// partial teardown below, and bridged drops cleanupErr and would
		// carry on into `nmcli connection modify` over the refused name. So
		// it is intercepted here, ahead of the split.
		//
		// The message stays generic about WHY the name was refused: the
		// wrapped error already names the reason, and there are three of them
		// (a name of the wrong shape, a device of the wrong kind, and a stat
		// that could not answer).
		var refusal storedNameRefusal
		if errors.As(cleanupErr, &refusal) {
			return fmt.Errorf("%s networking cannot start with this stored network configuration: %w. "+
				"Nothing was changed on the host: the refusal comes before the cleanup issues any command, so no connection was deleted and no interface was removed. "+
				"The name it refused was read from the state file in this config directory. "+
				"Correct or remove that name in the state file and start again, or start in a fresh config directory",
				mode, cleanupErr)
		}
		if cleanupErr != nil && mode == "shared" {
			return fmt.Errorf("shared networking cannot start until the leftover network configuration is gone, and removing it failed: %w. "+
				"The cleanup does not stop at its first failure, so every step after the one above was still attempted where it had anything to attempt, and the failure above names each one that failed. Several steps do nothing when there is nothing to do: the `ip link delete`s run only for an interface `ip link show` can see, and the reconnect runs only when an interface was found on the bridge AND this cleanup deleted the connection that would otherwise be reactivated over it -- so a reconnect can be skipped for a NIC that is on the bridge, and the line above this refusal says so when it is. After a reboot, where the connection keyfiles survive and the interfaces do not, the delete that failed above can be the only command this cleanup issued at all. "+
				"Where it did issue something, the host's networking has changed, and a physical interface can be left with no active connection -- `nmcli device status` shows which, and `sudo nmcli connection up <profile>` puts it back on the profile you name. Prefer that to `sudo nmcli device connect <iface>`, which activates whichever profile NetworkManager rates best for the device, routinely the bridge-slave profile after a bridged run -- one of the leftovers this cleanup was trying to remove. "+
				"The start is refused rather than attempted because shared mode enslaves no host interface: its bridge carries ipv4.method shared and its only port is meant to be the tap, and a leftover this tool could not remove may be what attaches a NIC to that bridge. "+
				"Remove the leftover the failure above names and start again, or use -network bridged, which rebuilds these connections itself",
				cleanupErr)
		}
		time.Sleep(staleCleanupSettleDelay)
	}
	return nil
}

// staleCleanupSettleDelay is how long the preflight waits after tearing down a
// stale bridge, giving NetworkManager time to finish releasing the interfaces
// before they are recreated. It is a var only so network_linux_test.go can
// zero it; nothing in production assigns it.
var staleCleanupSettleDelay = 2 * time.Second

// hasStaleBridgeResources reports whether a previous run left any part of this
// bridge behind: the bridge link, the bridge connection, the <bridge>-uplink
// connection or the <bridge>-tap connection.
//
// Every term matters to BOTH modes, including the -uplink one that only the
// bridged path ever creates. cleanupNMConnections attempts every delete and
// returns the ones that failed joined together, so a teardown can end with
// the bridge connection gone and the -uplink connection still on the host.
// That orphan carries `master <bridge> slave-type bridge` and autoconnect, so
// a `--network shared` run that brought its bridge up over it would have
// NetworkManager enslave the host's physical NIC to a NAT bridge -- which
// destroys the "no uplink is enslaved" invariant that is the entire reason
// shared mode exists, and takes the host's connectivity with it. Widening
// this predicate is what gets that orphan DELETED on a shared run, and
// linuxNetworkPreflight refuses the start when the delete it then attempts
// fails. The bridged path carries on over the same failure, because it
// recreates and re-modifies the -uplink connection itself.
//
// Seeing it here is not what ENFORCES the invariant, and nothing in this file
// should read as if it were: every probe in the expression below answers "no"
// when it cannot get an answer, and the three connection names it knows are
// not the only ones a profile can have. What enforces it is the bridge-port
// assertion in prepareLinuxSharedWithNM, which asks the kernel which
// interfaces are actually ports of the bridge. This predicate makes the
// common case a clean start instead of a refusal.
//
// linuxNetworkPreflight and HasStaleNetworkResources both call this so the
// narrower of the two predicates cannot drift back into existence.
//
// tapConn (M2) is this VM's own indexed tap connection name -- <bridge>-tap
// at index 0, byte-identical to what this predicate always checked, and
// <bridge>-tapN above it -- rather than a hardcoded <bridge>-tap, so a
// leftover from a crashed run at THIS index is what is detected, and a live
// sibling sitting at a different index is not read as staleness. siblingLive
// already gates whether this predicate is even consulted; see
// linuxNetworkPreflight.
func hasStaleBridgeResources(bridge, tapConn string) bool {
	return IsLinuxBridge(bridge) || nmConnectionExists(bridge) ||
		nmConnectionExists(bridge+"-uplink") || nmConnectionExists(tapConn)
}

// PrepareLinuxBridge prepares the host side of --network bridged for one VM,
// identified by index. siblingLive is true when another VM in this config
// dir is currently live, which skips the stale-cleanup branch that would
// otherwise tear down a bridge a sibling is using.
func PrepareLinuxBridge(st *state.State, runtimeDir string, index int, siblingLive bool) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	bridge, err := resolveLinuxBridgeName(st)
	if err != nil {
		return err
	}
	tap := TapNameForIndex(index)
	tapConn := TapConnNameForIndex(bridge, index)
	if err := linuxNetworkPreflight(st, runtimeDir, "bridged", bridge, tap, tapConn, siblingLive); err != nil {
		return err
	}

	uplink := st.Network.BridgeInterface
	if uplink == "" {
		// Retry a few times in case NM is still restoring the connection
		var u string
		var err error
		for i := 0; i < 5; i++ {
			u, err = detectDefaultUplink()
			if err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			return err
		}
		uplink = u
	}
	if uplink == bridge {
		return fmt.Errorf("uplink interface and bridge cannot be the same: %s", uplink)
	}
	if !linkExists(uplink) {
		return fmt.Errorf("uplink interface does not exist: %s", uplink)
	}
	if IsLinuxBridge(uplink) {
		return fmt.Errorf("uplink interface must be a physical host iface, got bridge: %s", uplink)
	}
	if isVirtualInterface(uplink) {
		return fmt.Errorf("uplink interface must be a physical host iface, got virtual interface: %s (use -bridge-if to specify a physical interface like eth0, enp*, or wlan*)", uplink)
	}

	if err := prepareLinuxBridgeWithNM(bridge, tap, tapConn, uplink); err != nil {
		return err
	}

	st.Network.Mode = "bridged"
	st.Network.BridgeName = bridge
	st.Network.BridgeInterface = uplink
	st.Network.TapName = tap
	// There is no NetworkManager DHCP server in bridged mode; the guest leases
	// from whatever server the physical network runs. Clear the field rather
	// than merely not writing it, for the same reason PrepareLinuxShared
	// clears BridgeInterface: `start --network shared` followed by
	// `start --network bridged` never runs cleanup in between, so an earlier
	// shared run leaves a lease path here, and the reader of this field would
	// report a previous guest's address as this one's.
	st.Network.DHCPLeaseFile = ""
	st.Network.CleanupRequired = true
	st.Network.CreatedByKairosLab = true
	st.Network.LastPreparedAt = state.NowRFC3339()
	return nil
}

func prepareLinuxBridgeWithNM(bridge, tap, tapConn, uplink string) error {
	if !nmcliAvailable() {
		return fmt.Errorf("NetworkManager is active but nmcli is not installed")
	}
	bridgeConn := bridge
	uplinkConn := bridge + "-uplink"
	uid, err := tapOwnerUID()
	if err != nil {
		return err
	}

	if !nmConnectionExists(bridgeConn) {
		if err := sudo("nmcli", "connection", "add", "type", "bridge", "ifname", bridge, "con-name", bridgeConn, "autoconnect", "yes", "stp", "no"); err != nil {
			return err
		}
	}
	if err := sudo("nmcli", "connection", "modify", bridgeConn, "connection.interface-name", bridge, "ipv4.method", "auto", "ipv6.method", "auto", "bridge.stp", "no", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if !nmConnectionExists(uplinkConn) {
		if err := sudo("nmcli", "connection", "add", "type", "ethernet", "ifname", uplink, "con-name", uplinkConn, "master", bridgeConn, "slave-type", "bridge", "autoconnect", "yes"); err != nil {
			return err
		}
	}
	if err := sudo("nmcli", "connection", "modify", uplinkConn, "connection.interface-name", uplink, "master", bridgeConn, "slave-type", "bridge", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if !nmConnectionExists(tapConn) {
		if err := sudo("nmcli", "connection", "add", "type", "tun", "ifname", tap, "con-name", tapConn, "mode", "tap", "owner", uid, "master", bridgeConn, "slave-type", "bridge", "autoconnect", "yes"); err != nil {
			return err
		}
	}
	if err := sudo("nmcli", "connection", "modify", tapConn, "connection.interface-name", tap, "tun.mode", "tap", "tun.owner", uid, "master", bridgeConn, "slave-type", "bridge", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if err := sudo("nmcli", "connection", "up", bridgeConn); err != nil {
		return err
	}
	// The bridge device exists from the activation above, which is the
	// earliest `ip link set <bridge> alias` can reach it. See
	// markBridgeAsOurs for why a failure here stops the start.
	if err := markBridgeAsOurs(bridge, bridgeConn); err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "up", uplinkConn); err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "up", tapConn); err != nil {
		return err
	}
	return nil
}

// PrepareLinuxShared builds the host side of --network shared: a bridge whose
// only port is the tap, carrying ipv4.method shared. NetworkManager then
// assigns 10.42.x.1/24 to the bridge, starts a DHCP server and DNS forwarder
// on it, and NATs the guests out of whatever the host's current default
// connection happens to be.
//
// Deliberately absent, for anyone looking for it: every uplink step
// PrepareLinuxBridge takes — detectDefaultUplink and its retry loop, the
// "must be a physical host iface" validations, the <bridge>-uplink connection.
// shared enslaves no physical interface, which is the entire point of the
// mode. A bridge with no port still activates (NetworkManager ignores carrier
// by default on controller types), so there is nothing to detect and nothing
// to validate. It is also why shared works over Wi-Fi where bridged cannot: no
// guest frame ever leaves the host with a MAC the access point did not see
// associate. And because the masquerade rule NetworkManager installs matches
// on the source subnet with no output-interface match, the host can move from
// Wi-Fi to Ethernet under a running VM without any rule churn.
func PrepareLinuxShared(st *state.State, runtimeDir string, index int, siblingLive bool) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	bridge, err := resolveLinuxBridgeName(st)
	if err != nil {
		return err
	}
	tap := TapNameForIndex(index)
	tapConn := TapConnNameForIndex(bridge, index)
	if err := linuxNetworkPreflight(st, runtimeDir, "shared", bridge, tap, tapConn, siblingLive); err != nil {
		return err
	}

	if err := prepareLinuxSharedWithNM(bridge, tap, tapConn, siblingLive); err != nil {
		return err
	}

	st.Network.Mode = "shared"
	st.Network.BridgeName = bridge
	// There is no uplink in shared mode. Clear the field rather than merely
	// not writing it: an earlier bridged run can have left an interface name
	// in state, and `status` would print it as this VM's uplink.
	st.Network.BridgeInterface = ""
	st.Network.TapName = tap
	// bridge, not tap: the lease file is named after the interface that
	// received ipv4.method shared. See sharedLeaseFilePath.
	st.Network.DHCPLeaseFile = sharedLeaseFilePath(bridge)
	st.Network.CleanupRequired = true
	st.Network.CreatedByKairosLab = true
	st.Network.LastPreparedAt = state.NowRFC3339()
	return nil
}

func prepareLinuxSharedWithNM(bridge, tap, tapConn string, siblingLive bool) error {
	if !nmcliAvailable() {
		return fmt.Errorf("NetworkManager is active but nmcli is not installed")
	}
	bridgeConn := bridge
	uid, err := tapOwnerUID()
	if err != nil {
		return err
	}

	// autoconnect is off on both connections this path creates, where the
	// bridged path leaves it on. ipv4.method shared is not an idle setting: a
	// bridge carrying it runs a DHCP server and a DNS forwarder, and turns on
	// IPv4 forwarding and a MASQUERADE rule. NetworkManager persists these
	// connections as keyfiles, so with autoconnect on, a user who chose shared
	// mode once would get all four on every subsequent boot, with no VM
	// running and nothing asking for them. kairos-lab brings both connections
	// up explicitly on every start -- `nmcli connection up` activates a
	// connection whose autoconnect is no, since autoconnect governs only what
	// NetworkManager starts on its own.
	//
	// It is a trade and not a free win. What autoconnect still buys is
	// re-activation on an event nothing here watches for: `systemctl restart
	// NetworkManager` under a running shared-mode guest leaves the bridge and
	// the tap down and takes the guest's network with them until the next
	// `start`, where the bridged path comes back on its own. That is the cost
	// accepted here, against a DHCP server, a DNS forwarder, IPv4 forwarding
	// and a MASQUERADE rule appearing on every boot of a host that chose
	// shared once. The recoverable failure is the better one.
	if !nmConnectionExists(bridgeConn) {
		if err := sudo("nmcli", "connection", "add", "type", "bridge", "ifname", bridge, "con-name", bridgeConn, "autoconnect", "no", "stp", "no"); err != nil {
			return err
		}
	}
	// ipv4.method shared is the whole mode: it is what makes NetworkManager
	// address the bridge, run dnsmasq on it and install the NAT rule. No
	// ipv4.addresses, ipv4.dns or ipv4.dns-search go with it — the defaults
	// are what we want, and NetworkManager's verify() rejects a shared
	// connection that carries dns settings at all.
	//
	// ipv6.method is ignore here where the bridged path uses auto. Nothing
	// configures IPv6 for these guests, and a bridge with no port has nothing
	// upstream to source router advertisements, so auto would only leave the
	// bridge soliciting an address that cannot arrive. ignore says what is
	// actually true.
	if err := sudo("nmcli", "connection", "modify", bridgeConn, "connection.interface-name", bridge, "ipv4.method", "shared", "ipv6.method", "ignore", "bridge.stp", "no", "connection.autoconnect", "no"); err != nil {
		return err
	}

	// The tap is built as the bridged path builds it -- the guest end is the
	// same either way, only what sits on the other side of the bridge differs
	// -- except for autoconnect, which is off here for the reason above: a tap
	// left to come up on its own at boot brings its controller, and therefore
	// the DHCP server and the NAT rule, up with it.
	//
	// Both steps run with ipv4.method shared already persisted to the bridge
	// connection, so neither may walk away from its own failure; see
	// revertAfterSharedWritten.
	if !nmConnectionExists(tapConn) {
		if err := sudo("nmcli", "connection", "add", "type", "tun", "ifname", tap, "con-name", tapConn, "mode", "tap", "owner", uid, "master", bridgeConn, "slave-type", "bridge", "autoconnect", "no"); err != nil {
			return revertAfterSharedWritten(bridgeConn, tapConn, siblingLive, err)
		}
	}
	if err := sudo("nmcli", "connection", "modify", tapConn, "connection.interface-name", tap, "tun.mode", "tap", "tun.owner", uid, "master", bridgeConn, "slave-type", "bridge", "connection.autoconnect", "no"); err != nil {
		return revertAfterSharedWritten(bridgeConn, tapConn, siblingLive, err)
	}

	// The port assertion. What it asks the kernel is which interfaces are
	// ports of this bridge; what it demands is that none of them is one this
	// start did not put there.
	//
	// The two calls made after a `connection up` returned success are
	// unconditional. The one before any activation goes through
	// refusePreexistingBridgePort, which has a question to settle first: a
	// clean first run has no device of this name at all -- both connections
	// above were added with autoconnect no and nothing has activated either
	// -- and a device that is not on the host has no ports for the probe to
	// read. So a clean start reads the port list TWICE, and a start that
	// arrives over a device that is already there reads it three times.
	//
	// Before either `connection up` the expected port list is EMPTY. Nothing
	// has attached the tap yet -- that is the last command in this function
	// -- so a bridge with any port at all at this point has one from
	// somewhere else. Expecting nothing is also what keeps st.Network.TapName
	// out of the predicate: an exception named by the untrusted file this
	// check exists to defend against is an exception that file can aim at the
	// host's own NIC, and validateStoredInterfaceName accepts "eth0" there.
	//
	// A bridge link that got past the preflight can already carry a host NIC:
	// linkExists answers "no" for any `ip` failure, so the `ip link delete` in
	// the teardown is skipped while the device is still in /sys with whatever
	// was on it still on it. The connection profile has just been re-pointed
	// at ipv4.method shared, so this is the last moment before that method is
	// applied to a bridge carrying somebody's NIC.
	if err := refusePreexistingBridgePort(bridge, bridgeConn, tapConn, tap, siblingLive); err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "up", bridgeConn); err != nil {
		return revertAfterSharedWritten(bridgeConn, tapConn, siblingLive, err)
	}
	// Again now the bridge is up, which is the check that catches the hazard
	// this mode is guarded against: a profile carrying `master <bridge>
	// slave-type bridge` with autoconnect on enslaves its interface the moment
	// the controller activates, whether the preflight's connection probes
	// could see that profile or not. The tap comes up only after this passes,
	// so a refused run never puts a guest on the bridge.
	if err := refuseForeignBridgePort(bridge, bridgeConn, tapConn, tap, tapNotYetAttached, siblingLive); err != nil {
		return err
	}
	// Marked only once the port assertion above has passed: that check is
	// what establishes this bridge is the one this start built, and a mark
	// written before it would vouch for a bridge the next line may refuse.
	// See markBridgeAsOurs for why a failure here stops the start.
	if err := markBridgeAsOurs(bridge, bridgeConn); err != nil {
		return revertAfterSharedWritten(bridgeConn, tapConn, siblingLive, err)
	}
	if err := sudo("nmcli", "connection", "up", tapConn); err != nil {
		return revertAfterSharedWritten(bridgeConn, tapConn, siblingLive, err)
	}
	// And once more with the tap attached, where the expected port list is
	// exactly the tap. `nmcli connection up` returns when the connection has
	// activated, and an interface attached in the instant after that is not
	// seen by the check before it; this one costs one more `ip` call and
	// narrows that window to the tap's own activation. It is the only one of
	// the three that names the tap, and the exemption it makes is bounded by
	// the check before it: that one is unconditional and demands an EMPTY
	// list, so anything on the bridge by now arrived during the tap's own
	// activation. A port that is not the tap is refused here as at the other
	// two.
	//
	// The window is narrowed and not closed: nothing reads the port list
	// again once this function returns, and nothing watches it while the VM
	// runs.
	if err := refuseForeignBridgePort(bridge, bridgeConn, tapConn, tap, tapAttached, siblingLive); err != nil {
		return err
	}
	return nil
}

// tapNotYetAttached and tapAttached spell out refuseForeignBridgePort's last
// argument at the call sites, which would otherwise be a bare true or false
// on three calls that differ in nothing else.
const (
	tapNotYetAttached = false
	tapAttached       = true
)

// refusePreexistingBridgePort is the port assertion made before anything this
// start created has been activated, where one question has to be settled
// before the port list is worth asking for: is there a device of this name on
// the host at all? A clean first run has none -- both connections above are
// added with autoconnect no, so nothing of that name exists until the
// explicit `connection up` -- and a device that is not on the host has no
// ports, so there is no list to read. Putting the question anyway would not
// be harmless: refuseForeignBridgePort fails closed on a probe error, so
// every clean first start would be refused.
//
// The only answer that skips the check is "no such file or directory". The
// predicate this replaced was isLinuxBridge, whose whole body is
// `err == nil`, and that collapses every way a stat can fail into the one
// false an absent device gets -- which skipped ALL THREE checks. Each of
// them refuses now.
//
// It is not the same stat, and the difference decides which failures are
// live. isLinuxBridge stats /sys/class/net/<name>/bridge; netDeviceExists
// stats /sys/class/net/<name>, for the reason the next paragraph gives. So
// the standing ENOTDIR example does not carry over: measured on a real host,
// /sys/class/net/bonding_masters is a regular file, which makes
// bonding_masters/bridge ENOTDIR under the old path and bonding_masters
// itself a clean "exists" under this one -- after which the port check runs
// and refuses, because `ip` has no port list for a name that is not a device
// and refuseForeignBridgePort fails closed on that. Under the device path
// ENOTDIR needs /sys/class/net itself to be a non-directory, and a symlink
// loop is not reachable in sysfs. The class this refusal actually covers is
// permission denied: a /sys/class/net this user cannot read answers EACCES
// for every name in it, and answered isLinuxBridge's false before.
//
// It asks about the DEVICE and not about /sys/class/net/<name>/bridge,
// because "is this a Linux bridge" is the wrong question to hang a port check
// on. `ip -o link show master <dev>` lists the ports of any master -- a bond,
// a team, a VRF, an OVS bridge -- while that sysfs entry exists only for a
// Linux bridge, so the narrower question answers "nothing to check here" for
// a master that exists and has the host's NIC on it. The name comes out of
// state.json, and validateStoredInterfaceName accepts "bond0" in it. "There
// is no device of this name" is the only fact that implies "no ports", and it
// is the fact this asks for.
func refusePreexistingBridgePort(bridge, bridgeConn, tapConn, tap string, siblingLive bool) error {
	exists, err := netDeviceExists(bridge)
	if err != nil {
		return fmt.Errorf("shared networking will not start over bridge %s, because this start could not tell whether a device of that name is already on this host: %w. "+
			"That is what decides whether the bridge's port list has to be read before anything is activated, and the reason to read it is that a device already there -- left by an earlier run, or never ours at all -- can already have a host interface attached to it. ipv4.method shared has just been written to connection %s, and bringing that connection up is what puts a DHCP server, IPv4 forwarding and a MASQUERADE rule on whatever the device is carrying. "+
			"The only answer that means \"no such device, and therefore no ports\" is \"no such file or directory\". Anything else is a question left unanswered, and shared mode's promise is not one this start may make unchecked. "+
			"`ls -ld /sys/class/net/ /sys/class/net/%s` shows what could not be read; a /sys/class/net this user cannot read is the cause that reaches this message. "+
			"%s. "+
			"Or use -network bridged, which attaches an interface to a bridge on purpose, or -network user, which builds no bridge at all",
			bridge, err, bridgeConn, bridge, revertSharedSetup(bridgeConn, tapConn, siblingLive))
	}
	if !exists {
		return nil
	}
	return refuseForeignBridgePort(bridge, bridgeConn, tapConn, tap, tapNotYetAttached, siblingLive)
}

// refuseForeignBridgePort enforces the invariant shared mode exists for: the
// only port this bridge ever has is the tap, and it has none at all until the
// tap is activated. It asks the kernel, through `ip -o link show master
// <bridge>`, rather than inferring the answer from what the teardown before
// it believed it had deleted -- those are different questions, and the second
// one has been wrong twice. A profile nmConnectionExists could not see (it
// reports "does not exist" for any nmcli exit it dislikes, a restarting
// NetworkManager included) is never deleted, so cleanup joins no errors and
// returns nil; and a profile named anything other than the three
// cleanupNMConnections knows about -- a "Wired connection 1" somebody pointed
// at this bridge -- survives a cleanup that fully succeeded. Both end with a
// preflight that saw a clean host and a NIC on a NAT bridge.
//
// It fails CLOSED, which is the whole reason bridgeSlaveLinks returns an
// error. A probe that cannot answer is not an answer, and this one gates a
// root-run network reconfiguration. `ip -o link show master` is missing from
// busybox's `ip`, which exits 2 with `either "dev" is duplicate`; from an
// iproute2 older than the filter; and from a host with no `ip` on PATH at
// all. Every one of those is a permanent property of the host rather than a
// transient failure, so reading them as "no ports" would leave the invariant
// unenforced for good on whole classes of machine -- while the port list is
// the only thing enforcing it.
//
// Nothing here is gated on the bridge existing. Two of the three call sites
// ask after `nmcli connection up <bridge>` has returned success, and the
// device's existence is established by that success, so a probe that cannot
// answer at either of those points is a probe failure and never an absent
// bridge. The third asks before any activation and goes through
// refusePreexistingBridgePort, which settles that question first and carries
// on to this function unless the answer is a definite no.
//
// Both refusals hand the host back the way they found it, as far as they can;
// see revertSharedSetup. The profile that did the attaching is left alone: it
// may be one this tool never created, and deleting a user's network
// configuration on their behalf is the failure mode this whole path exists to
// avoid.
func refuseForeignBridgePort(bridge, bridgeConn, tapConn, tap string, tapIsAttached, siblingLive bool) error {
	out, err := bridgeSlaveLinks(bridge)
	if err != nil {
		return fmt.Errorf("shared networking will not start over bridge %s, because the check that nothing is attached to it could not be run: `ip -o link show master %s` failed: %w. "+
			"A device of that name is on this host, so the question is a real one and this start could not answer it. Shared mode's promise is that no host interface is a port of its bridge -- that is what the consent prompt says, and ipv4.method shared is what makes getting it wrong expensive -- so a port list that cannot be read is a refusal and not a pass. "+
			"`ip` may not be on PATH at all; it may be busybox's `ip`, which has no `show master` filter; or it may be an iproute2 older than that filter. `ip -V` says which. "+
			"The same list is in `ls /sys/class/net/%s/brif` when that device is a Linux bridge, and that needs none of them: if it names an interface of yours, something is attaching that interface to this bridge; if it is empty, `sudo ip link delete %s` removes the leftover device and the next start builds its own. "+
			"%s. "+
			"Or use -network bridged, which attaches an interface to a bridge on purpose, or -network user, which builds no bridge at all",
			bridge, bridge, err, bridge, bridge, revertSharedSetup(bridgeConn, tapConn, siblingLive))
	}
	// A port is expected -- and therefore never refused -- when it is this
	// VM's own tap (once attached) or when it satisfies bridgePortExempt's
	// three-part test: a generated tap name, a tun/tap device (condition 2,
	// which is what keeps a renamed physical NIC from ever qualifying), owned
	// by the uid this start itself would create a tap as. That is a bounded
	// weakening, accepted in exchange for letting a second VM join a bridge a
	// live sibling already has a tap on -- a leftover from a crashed run used
	// to be refused by the old, stricter "the list must be empty" rule; it is
	// accepted now, and condition 2 is what still keeps a real host NIC out.
	var unexpected []string
	for _, port := range parseBridgePorts(out) {
		if tapIsAttached && port == tap {
			continue
		}
		if bridgePortExempt(port) {
			continue
		}
		unexpected = append(unexpected, port)
	}
	if len(unexpected) == 0 {
		return nil
	}
	// The message names every unexpected port and not the first one: a host
	// with two NICs on this bridge used to be told about one of them, and the
	// user who cleared that one was refused again on the next start naming
	// the second, with no hint there had been more. It also says what it
	// observed and no more than that -- a port list read out of the kernel --
	// because it observed no profile and used to claim one.
	expectation := fmt.Sprintf("this bridge is meant to carry only generated, kairos-lab-owned taps at this point, since the tap %s is attached by a later step of this same start", tap)
	if tapIsAttached {
		expectation = fmt.Sprintf("the only ports this bridge is meant to have are the tap %s and any other generated, kairos-lab-owned taps", tap)
	}
	return fmt.Errorf("shared networking will not run over bridge %s: `ip -o link show master %s` reports %s on it, and %s. "+
		"That port list is all this start observed. It is the kernel's own answer about what is attached to the bridge; which profile attached it, or whether any profile did, was not looked at and is not claimed here. "+
		"The likely cause is a NetworkManager profile carrying `master %s slave-type bridge`: `nmcli -f NAME,DEVICE,TYPE connection show --active` lists the active ones, `nmcli -f connection.master connection show <name>` confirms which bridge one attaches to, and deleting or re-pointing it is the fix. "+
		"There may be no such profile to find. A bridge left behind by an earlier run keeps whatever is on it across a NetworkManager restart with nothing live holding it there, and then `sudo ip link delete %s` -- which removes the bridge and releases every port on it -- or `sudo ip link set dev <iface> nomaster` for one of them is what clears it. "+
		"Put an interface back with `sudo nmcli connection up <profile>` naming the profile you want, and only once the offending one is gone or re-pointed: `nmcli device connect <iface>` activates whichever profile NetworkManager rates best for that device, and after a bridged run that is routinely the bridge-slave profile it left behind, which attaches the interface to a bridge again. "+
		"%s. "+
		"Then start again, or use -network bridged, which attaches an interface to its bridge on purpose",
		bridge, bridge, quoteNames(unexpected), expectation, bridge, bridge, revertSharedSetup(bridgeConn, tapConn, siblingLive))
}

// bridgePortExempt is a three-part test: a port already on the bridge is
// exempt from refusal only when all three hold --
//
//  1. the name parses as one this tool would itself generate for some VM
//     index (IsGeneratedTapName);
//  2. /sys/class/net/<name>/tun_flags is readable, which exists only for a
//     tun/tap device and is what keeps a renamed physical NIC from ever
//     qualifying, whatever it is named;
//  3. /sys/class/net/<name>/owner equals the uid this start's own tap would
//     be created with (tapOwnerUID).
//
// Any read failure means NOT exempt -- fail closed, the same direction as
// every other probe on this path.
func bridgePortExempt(name string) bool {
	if _, ok := IsGeneratedTapName(name); !ok {
		return false
	}
	if !tapSysfsTunFlagsReadable(name) {
		return false
	}
	owner, ok := tapSysfsOwnerUID(name)
	if !ok {
		return false
	}
	want, err := tapOwnerUID()
	if err != nil {
		return false
	}
	return owner == want
}

// tapSysfsTunFlagsReadable and tapSysfsOwnerUID are swappable seams over the
// two /sys reads bridgePortExempt's tun/tap and ownership checks need, for
// the same reason statNetDevice is one: a test drives them without a real tap
// device on the host running it. Nothing in production assigns them; tests
// restore the originals with t.Cleanup.
var tapSysfsTunFlagsReadable = func(name string) bool {
	_, err := os.Stat(filepath.Join(sysClassNet, name, "tun_flags"))
	return err == nil
}

var tapSysfsOwnerUID = func(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(sysClassNet, name, "owner"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// revertAfterSharedWritten turns a failure from one of the steps that run
// after `nmcli connection modify <bridge> ... ipv4.method shared` returned
// into an error that also says what was done about it.
//
// revertSharedSetup states the rule -- a start that does not finish has to
// leave the host no more dangerous than it found it -- and it is not only the
// refusals' rule. That method is in a NetworkManager keyfile the moment the
// modify returns, so any exit past it that simply returns its error leaves a
// persisted NAT-bridge profile behind. The next thing to activate that
// connection, which a profile carrying `master <bridge> slave-type bridge`
// with autoconnect on does by itself, puts its interface on a bridge running
// a DHCP server, IPv4 forwarding and a MASQUERADE rule with no VM anywhere
// near it. The activation timeout is the sharpest of these: nmcli exits
// non-zero AFTER NetworkManager has brought the bridge up, so the profile is
// persisted and the device is live.
//
// Not every caller has reached the point of creating the tap connection, and
// `nmcli connection delete` on a name that is not there fails.
// revertSharedSetup reports that as a failed delete rather than hiding it,
// which costs a clause in the message and keeps it from claiming more than
// happened.
//
// The modify itself is deliberately NOT wrapped in this. A modify that failed
// wrote nothing, and the bridge connection it names may be one that predates
// this start, so reverting there would delete a user's profile over a command
// that changed nothing.
func revertAfterSharedWritten(bridgeConn, tapConn string, siblingLive bool, cause error) error {
	return fmt.Errorf("shared networking setup failed after ipv4.method shared had already been written to connection %s: %w. "+
		"That setting is persisted as soon as nmcli returns, so this start tried to take it back off the host rather than leave it for the next thing that activates that connection: %s",
		bridgeConn, cause, revertSharedSetup(bridgeConn, tapConn, siblingLive))
}

// revertSharedSetup undoes what prepareLinuxSharedWithNM has already done to
// this host, and returns a sentence saying what it attempted and what of that
// failed. Every exit past the modify that writes ipv4.method shared reaches
// it: the three refusals above call it directly, and the four steps that can
// fail after that modify returned call it through revertAfterSharedWritten.
//
// Deactivating is not reverting. By the time any of those runs,
// `nmcli connection modify <bridge> ... ipv4.method shared` has returned and
// NetworkManager has written that to a keyfile, so an exit that only took the
// connection down would walk away leaving a persisted NAT-bridge profile
// behind -- and a refusal fires precisely when something else is attaching an
// interface to that bridge, ordinarily a profile with autoconnect on. The
// next time that interface comes up, NetworkManager activates its controller,
// and the interface lands on a bridge running a DHCP server, IPv4 forwarding
// and a MASQUERADE rule, with no VM anywhere near it. A refusal has to leave
// the host no more dangerous than it found it, and so does a failure.
//
// The down comes first, even though deleting a connection deactivates it too,
// because it is the step that releases the interface and it is worth having
// happened even when the delete after it fails.
//
// The deletes are unconditional rather than gated on nmConnectionExists,
// which reports "does not exist" for any nmcli exit it dislikes and would
// skip the delete that matters on exactly the hosts this path is defending.
// What the callers know instead is that the bridge connection was modified
// successfully, since that modify is what put ipv4.method shared on this host
// and is what makes a revert owed at all. The tap connection is a different
// case: the earliest caller runs at a point where it may never have been
// added, and a delete of a name that is not there fails and is reported below
// as a failed delete rather than hidden.
//
// Deleting is more than restoring, for a bridge connection that predates this
// start: what its settings were is recorded nowhere, and what this start
// overwrote cannot be put back. What goes is a profile named after the bridge
// in state.json, which is the same profile every teardown in this file
// deletes.
// siblingLive (M3) gates both destructive steps -- the "down" and the
// bridge's own delete -- and not only the bridge delete: with a live sibling
// VM's tap on this bridge, taking the bridge connection down would disturb
// it exactly as deleting it would. This VM's own tap connection is still
// deleted either way, since it is this failed start's own resource and never
// the sibling's.
func revertSharedSetup(bridgeConn, tapConn string, siblingLive bool) string {
	notes := make([]string, 0, 4)
	if siblingLive {
		notes = append(notes, fmt.Sprintf("connection %s was left alone, and so was the bridge device itself: a sibling VM's tap is still live on it, and taking either down would disturb that VM", bridgeConn))
	} else if err := sudo("nmcli", "connection", "down", bridgeConn); err != nil {
		notes = append(notes, fmt.Sprintf("taking connection %s down failed (%v), which is what a bridge this start never activated does; if it was up, it may be up still", bridgeConn, err))
	} else {
		notes = append(notes, fmt.Sprintf("connection %s was taken down, which should release what it had attached", bridgeConn))
	}
	targets := []string{tapConn}
	if !siblingLive {
		targets = append(targets, bridgeConn)
	}
	var deleted, failed []string
	bridgeDeleted := false
	for _, conn := range targets {
		if err := sudo("nmcli", "connection", "delete", conn); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", conn, err))
			continue
		}
		deleted = append(deleted, conn)
		if conn == bridgeConn {
			bridgeDeleted = true
		}
	}
	if len(deleted) > 0 {
		notes = append(notes, fmt.Sprintf("the connections this start had written were deleted (%s)", strings.Join(deleted, ", ")))
	}
	if len(failed) > 0 {
		notes = append(notes, fmt.Sprintf("deleting %s failed", strings.Join(failed, ", ")))
	}
	if siblingLive {
		notes = append(notes, fmt.Sprintf("the ipv4.method shared this start had written to %s stays on this host, because a sibling VM still needs that bridge", bridgeConn))
	} else if bridgeDeleted {
		notes = append(notes, fmt.Sprintf("so the ipv4.method shared this start had written to %s is off this host again", bridgeConn))
	} else {
		notes = append(notes, fmt.Sprintf("so the ipv4.method shared this start had written to %s is still on this host, and `sudo nmcli connection delete %s` is what removes it", bridgeConn, bridgeConn))
	}
	return strings.Join(notes, "; ")
}

// CleanupLinuxBridge tears down one VM's (v's) network resources. siblingLive
// reports whether any OTHER VM in this config dir is live (VMIsLive applied
// to each): when true, only v's own tap connection and tap device are removed --
// never the bridge connection, never the bridge link -- and st.Network is
// left intact, since CreatedByKairosLab must survive for whichever VM
// eventually does remove the bridge. st.Network is cleared only when the
// bridge itself was actually torn down.
func CleanupLinuxBridge(st *state.State, v state.VM, siblingLive bool) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if !st.Network.CreatedByKairosLab {
		return nil
	}
	bridgeConn := managedBridgeName(st.Network)
	tapDevice := TapNameForIndex(v.Index)
	tapConn := TapConnNameForIndex(bridgeConn, v.Index)

	// Stamped before the teardown runs, not after it, because the outcome
	// that needs this recorded is the one that returns early. On a failure
	// the stored bridge and tap are deliberately kept so the next run can
	// finish the job, and that retained state is the only thing a later run
	// reads; without the stamp it says the teardown was never attempted.
	attemptedAt := state.NowRFC3339()
	st.Network.LastCleanupAttemptAt = attemptedAt

	if err := cleanupNMConnections(bridgeConn, tapDevice, tapConn, siblingLive); err != nil {
		return err
	}
	if siblingLive {
		return nil
	}

	// The success path clears the network, which is right: nothing kairos-lab
	// created is on the host any more. The timestamp is not one of those
	// resources, so it is written back over the zero value rather than being
	// lost to it.
	st.Network = state.Network{LastCleanupAttemptAt: attemptedAt}
	return nil
}

// HasStaleNetworkResources checks if there are kairos-lab network resources
// that exist but aren't tracked in state (e.g., from a failed setup). It asks
// about index 0's names, matching the single-VM host this check has always
// run against: it fires before any VM record exists at all, so there is no
// VM to read an index from.
func HasStaleNetworkResources(st *state.State) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if st.Network.CreatedByKairosLab {
		return false
	}
	bridge, _, tapConn := StaleNetworkResourceNames(st)
	return hasStaleBridgeResources(bridge, tapConn)
}

// CleanupStaleNetworkResources removes kairos-lab network resources that
// aren't tracked in state, typically from a failed or interrupted setup, at
// index 0 -- see HasStaleNetworkResources for why.
func CleanupStaleNetworkResources(st *state.State) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	bridge, tapDevice, tapConn := StaleNetworkResourceNames(st)
	return cleanupNMConnections(bridge, tapDevice, tapConn, false)
}

// cleanupNMConnections tears down the bridge, the tap and the NetworkManager
// connections that go with them, and returns every step that failed, joined.
//
// It used to print each failure with fmt.Printf and return nil. That made a
// PARTIAL teardown indistinguishable from a complete one to every caller: the
// only non-nil return was the refusal below, so `reset` printed "reset
// complete" over a host that still had the bridge, the tap and their
// connections on it. The printing was the second half of the same problem --
// fmt.Printf writes to process stdout, past the io.Writer the app layer
// threads through reset and cleanup, so those warnings were invisible to the
// app layer and to every test at that level.
//
// Collecting does not mean stopping. Every step below is REACHED whatever the
// ones before it did: one connection that will not delete must not strand the
// tap, the bridge and the reconnect behind it. Reached is not the same as
// issued, and no caller may say it is: the two `ip link delete`s run only for
// an interface linkExists can see, and the reconnect only for an interface
// findBridgeSlave found on the bridge whose uplink connection this run
// deleted, so a teardown can reach every step and issue one command.
// cleanupNMConnections is sibling-aware (M3): when siblingLive is true, only
// this VM's own tap connection (tapConn) and tap device (tapDevice) are
// deleted, and neither the bridge connection nor the bridge link nor the
// uplink connection is ever touched, whatever nmConnectionExists or
// linkExists says about them -- a live sibling may be using every one of
// those. When siblingLive is false the full teardown below runs exactly as
// it always has.
func cleanupNMConnections(bridgeConn, tapDevice, tapConn string, siblingLive bool) error {
	// Every destructive command below is built from these names, which come
	// out of state.json -- a 0644 file any process running as the user can
	// write. linuxNetworkPreflight validates them before a start, but reset
	// and cleanup reach here without passing through the preflight, so the
	// same check has to sit at the choke point too. The tap device name is
	// checked for exactly the same reason as the bridge name: it is the
	// argument of an `ip link delete` below. tapConn is not separately
	// validated here: it is always TapConnNameForIndex's own output, built
	// from a bridge name that has already passed this same check and an
	// integer index, never a string read fresh from state.json.
	//
	// Two checks per name, because the shape of a name does not say enough:
	// "eth0" is a valid name, and deleting it takes the host off the network
	// (kairos-io/kairos#5051). refuseForeignStoredDevice is what stops that,
	// by looking at the device the name resolves to rather than at the name.
	//
	// The bridge gets a third question on top of those two, and it is the one
	// about OWNERSHIP: "docker0", "virbr0" and a hypervisor host's own "br0"
	// are all real Linux bridges, so the kind check passes them and the
	// teardown took the containers or guests behind them off the network
	// (kairos-io/kairos#5288). refuseForeignStoredBridge asks instead whether
	// this bridge carries a mark kairos-lab put on it at creation.
	if err := validateStoredInterfaceName("bridge name", bridgeConn); err != nil {
		return refuseStoredName(err)
	}
	if err := refuseForeignStoredBridge("bridge name", bridgeConn); err != nil {
		return refuseStoredName(err)
	}
	tap := tapDevice
	if tap == "" {
		tap = DefaultTapName
	}
	if err := validateStoredInterfaceName("tap name", tap); err != nil {
		return refuseStoredName(err)
	}
	if err := refuseForeignStoredDevice("tap name", tap, isTapDevice, "a tun/tap device"); err != nil {
		return refuseStoredName(err)
	}
	if tapConn == "" {
		tapConn = bridgeConn + "-tap"
	}
	uplinkConn := bridgeConn + "-uplink"

	var failures []error

	if siblingLive {
		if nmConnectionExists(tapConn) {
			if err := sudo("nmcli", "connection", "delete", tapConn); err != nil {
				failures = append(failures, fmt.Errorf("delete connection %s: %w", tapConn, err))
			}
		}
		if linkExists(tap) {
			if err := sudo("ip", "link", "delete", tap); err != nil {
				failures = append(failures, fmt.Errorf("delete interface %s: %w", tap, err))
			}
		}
		return errors.Join(failures...)
	}

	// Find the physical interface enslaved to the bridge before we delete
	// anything. The tap is a port of this bridge too and must not be mistaken
	// for it, which is why the name is passed down; see parseBridgeSlave.
	var uplinkIface string
	if IsLinuxBridge(bridgeConn) {
		uplinkIface = findBridgeSlave(bridgeConn, tap)
	}

	// Delete all NM connections - use sudo (not sudoQuiet) so user can see
	// what's happening and errors are visible.
	//
	// The gate stays, although revertSharedSetup deletes these same three
	// names unconditionally and for a stated reason. The two return values
	// are read differently: revertSharedSetup's is prose inside an error
	// already being returned, so a delete of a name that is not there costs
	// it one clause, while this one's is a decision -- a non-nil return is
	// what `reset` prints in place of "reset complete", and what makes
	// linuxNetworkPreflight refuse a shared start. `nmcli connection delete`
	// fails for a name that is not on the host, and after every shared run
	// <bridge>-uplink is exactly that, since shared creates none. Deleting
	// unconditionally here therefore reports failure for the ordinary
	// teardown of the default mode, and refuses a shared start over a
	// leftover bridge device it has just removed successfully. Neither shows
	// up against fakeHost, which fails no command it is not told to; both are
	// what a host with nmcli on it does.
	//
	// What the gate costs is that nmConnectionExists reports "does not exist"
	// for any nmcli exit it dislikes, a restarting NetworkManager included,
	// so a profile that is really there can survive this loop with nothing
	// issued for it. That is why the reconnect below asks whether the uplink
	// connection was deleted instead of assuming these three are gone.
	uplinkConnDeleted := false
	for _, conn := range []string{tapConn, uplinkConn, bridgeConn} {
		if !nmConnectionExists(conn) {
			continue
		}
		if err := sudo("nmcli", "connection", "delete", conn); err != nil {
			failures = append(failures, fmt.Errorf("delete connection %s: %w", conn, err))
			continue
		}
		if conn == uplinkConn {
			uplinkConnDeleted = true
		}
	}

	// Now clean up any lingering interfaces that NM didn't remove
	if linkExists(tap) {
		if err := sudo("ip", "link", "delete", tap); err != nil {
			failures = append(failures, fmt.Errorf("delete interface %s: %w", tap, err))
		}
	}
	if linkExists(bridgeConn) {
		if err := sudo("ip", "link", "delete", bridgeConn); err != nil {
			failures = append(failures, fmt.Errorf("delete interface %s: %w", bridgeConn, err))
		}
	}

	// Reconnect the physical interface the bridge had enslaved, which the
	// deletes above leave with no active connection -- but only when this
	// teardown is what removed the <bridge>-uplink connection.
	//
	// That connection is the profile the bridged path writes, and it carries
	// `master <bridge> slave-type bridge` with autoconnect on. `nmcli device
	// connect <iface>` activates whichever profile NetworkManager rates best
	// for the device, which after a bridged run is routinely that one -- the
	// two messages this package prints about the command say so, and this is
	// the only place the tool runs it itself. Issued while that profile is
	// still on the host, it attaches the interface to the bridge again, and
	// this teardown then joins no errors and reports success: a cleanup that
	// put back what it had just removed and called it done.
	//
	// Not deleted is not the same as not there. The connection may never have
	// been on this host, the probe above may have been blind to it, or its
	// delete may have failed and be among the failures below. Only the first
	// is safe to reconnect over, and nothing here can tell the three apart,
	// so the interface is left to NetworkManager's own autoconnect and the
	// user is told which interface and which connection -- see
	// reconnectSkippedNotice.
	//
	// A shared teardown ordinarily reaches neither branch, because the only
	// port its own run attaches is the tap and findBridgeSlave never names
	// that. A host interface on that bridge anyway is the state the
	// start-time port assertions exist to refuse, and a teardown that finds
	// one chooses between these two branches the way a bridged one does.
	//
	// %q on both, for the reason quoteNames gives: uplinkIface came out of
	// `ip -o link show master` through findBridgeSlave and passed no
	// validator on the way, while the bridge and tap names either side of it
	// went through validateStoredInterfaceName. dev_valid_name() bars only an
	// empty name, IFNAMSIZ bytes or more, "." and "..", and any '/', ':' or
	// whitespace, so an interface really can be named with a raw ESC in it --
	// and it goes straight to a terminal, where "\x1b[2K\x1b[1G" erases the
	// line just written and returns the cursor to column 1.
	//
	// Three paths carry this value there on the branch that reconnects, not
	// the two escaped on these lines. The third is inside the %w: sudoError
	// builds its message from the same argv this name is a word of, so the
	// wrap below used to print one escaped copy and one raw copy of it in a
	// single string. That copy is renderArgv's to escape, and escaping it
	// here would not have reached it. The other branch has a path of its own,
	// and reconnectSkippedNotice escapes every copy in it.
	if uplinkIface != "" {
		if uplinkConnDeleted {
			fmt.Printf("Reconnecting %q...\n", uplinkIface)
			if err := sudo("nmcli", "device", "connect", uplinkIface); err != nil {
				failures = append(failures, fmt.Errorf("reconnect %q: %w", uplinkIface, err))
			}
		} else {
			fmt.Println(reconnectSkippedNotice(uplinkIface, uplinkConn))
		}
	}

	// nil when failures is empty, which is the whole point: a teardown that
	// did everything asked of it still reports success.
	return errors.Join(failures...)
}

// reconnectSkippedNotice is what a teardown prints instead of running `nmcli
// device connect <iface>`, when the connection that command could activate
// was not deleted by this run.
//
// It is a plain function for the reason sudoError is: the wording is not the
// subprocess, and a fake that replaced it would leave the sentence a user
// actually gets unexercised. Every copy of the interface name in it is
// escaped, because that name came off the kernel and passed no validator;
// the comment at the call site says why that matters.
//
// It says which connection was not deleted and not why. A delete that was
// issued and failed is named in the error the teardown returns; one that was
// never issued has nothing to report but this. Both end with that profile
// possibly still on the host, which is the fact this sentence is about.
func reconnectSkippedNotice(iface, uplinkConn string) string {
	return fmt.Sprintf("Not reconnecting %q: this cleanup did not delete connection %s, so that profile may still be on this host. "+
		"`nmcli device connect %q` activates whichever profile NetworkManager rates best for the device, routinely that one after a bridged run, which would attach the interface to a bridge again. "+
		"`nmcli device status` shows whether %q has an active connection, and `sudo nmcli connection up <profile>` puts it on the profile you name.",
		iface, uplinkConn, iface, iface)
}

var linkExists = func(name string) bool {
	if name == "" {
		return false
	}
	cmd := exec.Command("ip", "link", "show", name)
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// bridgeSlaveLinks returns the output of `ip -o link show master <bridge>`,
// one line per interface attached to that bridge, and whatever the command
// failed with.
//
// The error IS the signature's point. This used to answer "" for any failure,
// which made "the bridge has no ports" and "this host cannot answer the
// question" the same value -- and to refuseForeignBridgePort the first of
// those means PROCEED. The second one is permanent on a host whose `ip` is
// busybox's, which has no `show master` filter and exits 2 with `either "dev"
// is duplicate, or "br0" is garbage`; on an iproute2 older than the filter;
// and on a host with no `ip` on PATH. So that shape left the invariant
// unenforced for good on every such machine, while the port list was the only
// thing left enforcing it. Each caller now decides for itself: the assertion
// refuses on the error, the teardown carries on past it.
//
// This is the swappable seam, and it sits one level BELOW findBridgeSlave,
// which used to be the var itself. Replacing the whole function also replaced
// the part that decides WHICH interface gets reconnected, so the filter in it
// was never executed by a test: deleting that filter left the entire suite
// green. With only the exec replaced, every test that reaches a teardown runs
// the real parse and the real filter over output the fake supplies.
var bridgeSlaveLinks = func(bridge string) (string, error) {
	out, err := exec.Command("ip", "-o", "link", "show", "master", bridge).Output()
	if err != nil {
		// What `ip` printed is what tells "no such device" from "unknown
		// option" from "operation not permitted", and the refusal built on
		// this error asks the user to tell exactly those apart. %q because
		// these are another program's bytes on their way to a terminal.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%w: %q", err, firstLine(string(exitErr.Stderr), maxProbeStderrLen))
		}
		return "", err
	}
	return string(out), nil
}

// maxProbeStderrLen caps how much of a failing probe's stderr is quoted into
// an error. One line of `ip` diagnostics is what is wanted out of it; the
// rest of the message has to stay readable beside it.
const maxProbeStderrLen = 200

// findBridgeSlave finds the physical interface enslaved to the given bridge,
// or "" when it has none. tap is the tap device in play; it is a port of this
// same bridge and is never the answer. The choosing is parseBridgeSlave's, in
// network_shared_parse.go, where it can be tested on any host.
//
// A probe that failed answers "" here, and that tolerance is deliberate:
// cleanupNMConnections is this function's only caller, and what it wants is a
// reconnect hint, not a security control. The cost of a wrong "" there is one
// `nmcli device connect` not issued at the end of a teardown that has already
// deleted the connections and the links, where a teardown that refused to run
// because `ip` could not answer would instead leave the bridge, the tap and
// their profiles standing. The check that must not fail open calls
// bridgeSlaveLinks itself and refuses on the error; see
// refuseForeignBridgePort.
func findBridgeSlave(bridge, tap string) string {
	out, err := bridgeSlaveLinks(bridge)
	if err != nil {
		return ""
	}
	return parseBridgeSlave(out, tap)
}

// sudo, bridgeSlaveLinks above it and the host probes further down this file
// are package-level vars rather than plain functions so network_linux_test.go
// can swap them for in-process fakes and assert the exact argv sequence these
// paths hand to root. Each is one exec call and its result -- no branch, no
// path and no wording any caller depends on. So what a fake replaces is the
// subprocess and never a decision: findBridgeSlave, which decides which
// interface a teardown reconnects, is a plain function for that reason, and
// so are refuseForeignBridgePort, which decides what the port list means,
// sudoError below, which words the failure every caller of sudo wraps, and
// netDevicePath, which decides WHICH path the device stat asks about.
// bridgeSlaveLinks is the one that still words its own error, and it is
// worded from bytes the subprocess produced. Nothing in production assigns
// these; the tests restore the originals with t.Cleanup.
var sudo = func(name string, args ...string) error {
	argv := append([]string{name}, args...)
	cmd := exec.Command("sudo", argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return sudoError(argv, err)
	}
	return nil
}

// sudoError is the error a failed root command reports. It is a plain
// function outside the var above because it is not part of the subprocess:
// it is the wording every caller of sudo wraps its own message around, and a
// fake that returned its failure bare would leave that wording unexercised.
// network_linux_test.go's fake host calls this for the same reason it calls
// the real parse -- so the message a test measures is the message a user
// gets.
func sudoError(argv []string, err error) error {
	return fmt.Errorf("sudo command failed: sudo %s: %w", renderArgv(argv), err)
}

func IsPathGone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// IsLinuxBridge is a function and not a var so that it is the same kind of
// identifier as the !linux IsLinuxBridge in network_stub.go. When this was an
// exported var, `vm.IsLinuxBridge = f` compiled on Linux and failed to compile
// on darwin, which is a GOOS-specific break waiting for the first caller
// outside this package to write it. The swappable seam the tests need stays,
// one level down and unexported.
func IsLinuxBridge(name string) bool {
	return isLinuxBridge(name)
}

var isLinuxBridge = func(name string) bool {
	if runtime.GOOS != "linux" || name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(sysClassNet, name, "bridge"))
	return err == nil
}

// sysClassNet is the directory the kernel lists network devices in. A device
// named N is on this host exactly when /sys/class/net/N is there, whatever
// kind of device it is; the "bridge" entry isLinuxBridge stats one level
// further down exists only for a Linux bridge.
const sysClassNet = "/sys/class/net"

// wirelessDevicePaths returns the two /sys entries whose presence says the
// named device is a Wi-Fi radio, in the order they are asked about.
//
// The paths are out here, in production code a test can call, and not inside
// isWiFiIface below, for the reason netDevicePath gives: every test swaps the
// stat seam, so a path built behind it is a path no test ever executes, and
// the path IS the decision here. /sys/class/net/<name>/type would compile and
// pass a suite while answering 1 for a Wi-Fi device in managed mode, which is
// every Wi-Fi device a user bridges.
//
// Both entries, because neither alone covers the drivers in use:
//
//   - phy80211 is a symlink to the cfg80211 wiphy, added by
//     wiphy_register/ieee80211_register_hw, so every mac80211 and every
//     full-MAC cfg80211 driver has it. This is the modern answer.
//   - wireless is the older WEXT directory, published by net_device's
//     wireless_handlers. Drivers still shipping only that -- and the ndiswrapper
//     and vendor out-of-tree modules a user may be on -- have no phy80211.
//
// A device that is neither is not a radio, which is the answer an ordinary
// Ethernet NIC needs.
func wirelessDevicePaths(name string) []string {
	device := netDevicePath(name)
	return []string{
		filepath.Join(device, "phy80211"),
		filepath.Join(device, "wireless"),
	}
}

// isWiFiIface reports whether the named host interface is a Wi-Fi radio.
//
// It is the Linux half of the detector network_notdarwin.go's IsWiFiIface
// delegates to, and it gates a WARNING and never a refusal, which is what
// decides how it fails: a stat that cannot be read answers "not Wi-Fi", so an
// unreadable /sys costs the user a caveat rather than the run. That is the
// opposite of refusePreexistingBridgePort's fail-closed reading of the same
// kind of error, and deliberately so -- that one gates a root-run network
// reconfiguration, this one gates a sentence.
//
// statNetDevice is reused rather than given a seam of its own. It holds no
// decision -- not the path, which is wirelessDevicePaths's above, and not the
// classification, which is the loop's -- so it is the same stat asked about a
// different path, which is all a second seam would have been.
func isWiFiIface(name string) bool {
	if name == "" {
		return false
	}
	for _, path := range wirelessDevicePaths(name) {
		if statNetDevice(path) == nil {
			return true
		}
	}
	return false
}

// netDevicePath returns the /sys entry whose presence answers "is a device of
// this name on this host".
//
// The join is out here, in production code a test can call, and not inside
// statNetDevice below, because the PATH is the decision and the stat is not.
// Every test swaps that var, so a path built inside it is a path no test ever
// executes: appending "bridge" to it -- reinstating the predicate the device
// stat replaced, which answers "not there" for a bond, a team, a VRF or an
// OVS bridge that is very much there with the host's NIC on it -- left the
// entire suite green. The seam is handed a finished path now, and
// TestNetDeviceExistsStatsTheDeviceEntry reads it.
func netDevicePath(name string) string {
	return filepath.Join(sysClassNet, name)
}

// statNetDevice stats an already-built path and returns whatever that stat
// said. It is the swappable seam, and like sudo and bridgeSlaveLinks it holds
// nothing a caller depends on: not the classification of the error, which is
// the whole point of the fix it belongs to and is netDeviceExists's, and not
// the path, which is netDevicePath's. Either one inside here would be a
// decision the tests replace instead of run.
var statNetDevice = func(path string) error {
	_, err := os.Stat(path)
	return err
}

// netDeviceExists answers whether a network device of this name is on the
// host, and hands back the stat's error when it cannot tell.
//
// It is the honest form of the decision isLinuxBridge above used to carry as
// a bare bool -- a different path, asked in a different shape. There,
// `err == nil` makes an unreadable /sys/class/net indistinguishable from a
// device that is not there; both of that function's callers want the
// fail-open reading of that -- hasStaleBridgeResources is looking for a
// reason to run a cleanup, and cleanupNMConnections for an interface to
// reconnect -- and the refusal that gates a root-run network reconfiguration
// does not. So the error is returned rather than swallowed, and
// refusePreexistingBridgePort decides what to do with it.
//
// Only os.ErrNotExist is a definite no, and it is a strong one: a device that
// is not in /sys/class/net is not on the host, so it has no ports and there
// is nothing to check. Every other stat failure is the absence of an answer.
//
// A /sys that is not mounted lands on the definite no, and that is why it is
// written down here and not in refusePreexistingBridgePort's message: /sys is
// then an empty mount point, the stat walks into a missing /sys/class and
// returns ENOENT -- measured -- exactly as it does for a name no device has,
// so that refusal is never printed for it. The start proceeds instead, into
// the two port checks prepareLinuxSharedWithNM makes unconditionally after
// each `connection up`, both of which refuse when their probe fails.
//
// Asking about the device rather than about /sys/class/net/<name>/bridge is
// what makes the "no" mean what this needs it to mean: a bond, a team, a VRF
// and an OVS bridge are all masters whose ports `ip -o link show master`
// lists, and that "bridge" entry is a Linux bridge's -- which is the premise
// isLinuxBridge above is built on, and the reason it cannot carry this.
func netDeviceExists(name string) (bool, error) {
	if name == "" {
		return false, errors.New("no interface name to look for")
	}
	switch err := statNetDevice(netDevicePath(name)); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// storedNameRefusal marks the refusals cleanupNMConnections makes over a name
// it was given, BEFORE it issues any command.
//
// It exists because the two things cleanupNMConnections can return are the
// opposite of each other and its callers cannot otherwise tell them apart. A
// joined failure means the teardown ran and some of it did not work, so the
// host's networking has changed in a way nobody has a full account of. A
// refusal means nothing ran at all: the host is exactly as it was, and what
// is wrong is the stored configuration.
//
// The wrapped error is returned unchanged by Error, so the marker costs the
// message nothing; it is read with errors.As, never printed.
type storedNameRefusal struct{ err error }

func (r storedNameRefusal) Error() string { return r.err.Error() }
func (r storedNameRefusal) Unwrap() error { return r.err }

// refuseStoredName is the one way cleanupNMConnections refuses a name it was
// handed, so that every such refusal carries the marker and no later caller
// has to guess which returns are which.
func refuseStoredName(err error) error {
	return storedNameRefusal{fmt.Errorf("refusing to clean up network resources: %w", err)}
}

// refuseForeignStoredDevice refuses a stored interface name that names a
// device of the wrong KIND on this host: something is there under that name,
// and it is not the kind of device the caller says kairos-lab builds.
//
// It pairs with validateStoredInterfaceName, which runs just before it and is
// a rule about the SHAPE of a name. That rule accepts "eth0", "bond0" and
// every other real interface name, so this is the other half: a rule about the
// DEVICE the name resolves to.
//
// Three answers and not two, because "no such device" is not evidence of a
// foreign one. An interrupted setup routinely leaves a NetworkManager profile
// behind with no device of that name left to go with it, and the stale cleanup
// exists to remove exactly that, so a name that resolves to nothing has to
// pass. A stat that cannot answer refuses: the commands behind this guard run
// under sudo and delete, so an unanswered question is not a yes.
//
// What this does NOT establish is ownership. A device of the right kind passes
// whoever made it. For the bridge -- the name whose deletion takes a whole
// host network down -- refuseForeignStoredBridge below asks that second
// question; this one is left to the tap, where the kind check is the whole
// guard for the reason isTapDevice gives.
func refuseForeignStoredDevice(field, name string, ours func(string) bool, want string) error {
	exists, err := netDeviceExists(name)
	if err != nil {
		return fmt.Errorf("invalid %s %q in stored configuration: cannot tell what kind of device this is: %w", field, name, err)
	}
	if !exists || ours(name) {
		return nil
	}
	return fmt.Errorf("invalid %s %q in stored configuration: %s is a device on this host and is not %s, so this teardown will not delete it", field, name, name, want)
}

// isTapDevice reports whether name is a tun/tap device, by the presence of the
// tun_flags attribute the tun driver publishes and no other link type has. It
// is deliberately weaker than bridgePortExempt, which also requires a
// kairos-lab-generated name and this user's ownership: this is a refusal at a
// teardown choke point, and a state.json written by an older version records
// tap names that predate the generated-name scheme.
func isTapDevice(name string) bool {
	return name != "" && tapSysfsTunFlagsReadable(name)
}

func detectDefaultUplink() (string, error) {
	candidates := DetectUplinkCandidates()
	if len(candidates) == 0 {
		return "", fmt.Errorf("could not determine default uplink interface (all candidates are bridges, virtual, or loopback)")
	}
	return candidates[0], nil
}

// DetectUplinkCandidates returns all valid physical interfaces from the default routes.
// The list preserves the order returned by `ip route show default` after filtering and de-duplication.
func DetectUplinkCandidates() []string {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var candidates []string
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "dev" {
				iface := fields[i+1]
				if iface != "" && iface != "lo" && !IsLinuxBridge(iface) && !isVirtualInterface(iface) && !seen[iface] {
					seen[iface] = true
					candidates = append(candidates, iface)
				}
			}
		}
	}
	return candidates
}

func isVirtualInterface(name string) bool {
	virtualPrefixes := []string{
		"docker",  // Docker default bridge
		"br-",     // Docker custom networks
		"veth",    // Virtual ethernet (container endpoints)
		"virbr",   // libvirt bridges
		"vnet",    // libvirt VM interfaces
		"lxcbr",   // LXC bridges
		"lxdbr",   // LXD bridges
		"cni",     // Kubernetes CNI
		"flannel", // Kubernetes flannel
		"calico",  // Kubernetes calico
		"weave",   // Kubernetes weave
		"tunl",    // IPIP tunnels
		"podman",  // Podman networks
	}
	for _, prefix := range virtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

var networkManagerActive = func() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", "NetworkManager").Run() == nil
}

// nmcliAvailable reports whether the nmcli binary is on PATH. It is separate
// from nmConnectionExists so a test can drive the prepare paths on a host that
// has no NetworkManager installed at all.
var nmcliAvailable = func() bool {
	_, err := exec.LookPath("nmcli")
	return err == nil
}

var nmConnectionExists = func(name string) bool {
	if name == "" {
		return false
	}
	if _, err := exec.LookPath("nmcli"); err != nil {
		return false
	}
	return exec.Command("nmcli", "-t", "-f", "NAME", "connection", "show", name).Run() == nil
}

// tapOwnerUID resolves the uid the tap device is handed to, so QEMU can open
// it without privileges. Under sudo the invoking user is the one that matters
// and not root, which is why SUDO_USER is consulted first and why uidForUser
// exists at all.
//
// Everything else falls through to os.Getuid(), the kernel's own answer for
// who this process is: no subprocess, and nothing to validate. There is
// deliberately no $USER step. $USER is set by login shells and by little
// else, so under a systemd unit, cron, `env -i` or a minimal container shell
// it is absent -- and a chain ending in a literal "root" then handed the tap
// to uid 0 while the process ran as somebody else, leaving QEMU unable to
// open the device it had just asked to have created.
func tapOwnerUID() (string, error) {
	if user := os.Getenv("SUDO_USER"); user != "" {
		return uidForUser(user)
	}
	return strconv.Itoa(os.Getuid()), nil
}

func uidForUser(user string) (string, error) {
	out, err := exec.Command("id", "-u", user).Output()
	if err != nil {
		return "", fmt.Errorf("resolve uid for user %q: %w", user, err)
	}
	uid := strings.TrimSpace(string(out))
	if uid == "" {
		return "", fmt.Errorf("resolve uid for user %q: empty uid", user)
	}
	for _, r := range uid {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("resolve uid for user %q: invalid uid %q", user, uid)
		}
	}
	return uid, nil
}

// The two marks kairos-lab puts on a bridge it creates, and reads back before
// it will delete one.
//
// kairos-io/kairos#5288: the kind check refuseForeignStoredDevice makes asks
// what a name resolves to, never who made it, so "docker0", "virbr0" and a
// hypervisor host's own "br0" all pass it and reach `sudo ip link delete`.
// Nothing inside state.json can close that: the file is 0644, so a flag in it
// vouching for the name beside it vouches for whatever that name was changed
// to. The mark has to be set on the host at creation and read back from the
// host at teardown.
//
// Two marks rather than one, because each covers the other's hole:
//
//   - The device alias is on the DEVICE, which is the thing `ip link delete`
//     destroys, and it survives the NetworkManager profile being removed --
//     the half-cleaned host the stale cleanup exists for. It does not survive
//     a reboot: the bridge link is gone and NetworkManager rebuilds it from
//     the keyfile, which carries no alias.
//   - The user.data item is in the KEYFILE, so it does survive a reboot, and
//     it is the one that answers after a bridged run whose `autoconnect yes`
//     brought the bridge back by itself. It says nothing about a device whose
//     profile has been deleted.
//
// A bridge is ours when EITHER answers yes, so the pair covers a reboot and a
// deleted profile both. Neither is on docker0, virbr0 or a host br0: Docker
// and libvirt build their bridges directly and NetworkManager has no profile
// for them at all, and a br0 the host's admin made has a profile without this
// item in it.
//
// The key is a valid NetworkManager user data key: libnm requires one '.' and
// allows letters, digits and '-' (nm_setting_user_check_key).
const (
	bridgeOwnerAlias     = "kairos-lab"
	bridgeOwnerMarkKey   = "kairos-lab.created"
	bridgeOwnerMarkValue = "1"
)

// markBridgeAsOurs puts both marks on a bridge this run has just brought up.
//
// It runs on every start and not only on the one that creates the bridge, so
// a bridge built by a kairos-lab older than this change is marked by the next
// start over it, and a mark a reboot dropped is put back.
//
// A failure here fails the start. The alternative is a VM running on a bridge
// no later teardown will agree to remove, which is a trap the user cannot see
// until they try to clean up.
func markBridgeAsOurs(bridge, bridgeConn string) error {
	if err := sudo("ip", "link", "set", bridge, "alias", bridgeOwnerAlias); err != nil {
		return err
	}
	// `+user.data` adds this item to whatever the profile already carries,
	// where a bare `user.data` would replace the whole dictionary. The
	// bridged path reuses a profile of this name when it finds one, so the
	// dictionary is not necessarily this tool's to overwrite.
	return sudo("nmcli", "connection", "modify", bridgeConn, "+user.data", bridgeOwnerMarkKey+"="+bridgeOwnerMarkValue)
}

// netDeviceIfalias reads a device's alias, the string `ip link set <dev> alias`
// writes. The bool is whether the read answered at all: a device with no alias
// set reads back as empty, and an unreadable /sys reads back as nothing known.
// Neither is a mark, so both get the same treatment from the only caller.
var netDeviceIfalias = func(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(sysClassNet, name, "ifalias"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// nmConnectionUserData returns the raw `nmcli -g user.data connection show`
// output for a profile. Parsing it is userDataHasItem's, for the reason
// netDevicePath gives about the device stat: the seam holds the subprocess and
// nothing a caller depends on, so the decision stays in code a test executes.
//
// No sudo. user.data is not a secret, so an unprivileged nmcli reads it, and a
// teardown that has not yet decided it may delete anything should not be
// asking the user for a password.
var nmConnectionUserData = func(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if _, err := exec.LookPath("nmcli"); err != nil {
		return "", false
	}
	out, err := exec.Command("nmcli", "-g", "user.data", "connection", "show", name).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// userDataHasItem reports whether nmcli's rendering of a user.data dictionary
// carries this key with this value.
//
// nmcli prints the dictionary as items separated by commas, each one a key and
// a value around an '='. How much whitespace it puts around the '=' has
// differed between versions, so both sides are trimmed rather than matched
// exactly. The separator is safe to split on: nmcli escapes a comma that is
// part of a value, and neither this key nor this value contains one.
func userDataHasItem(out, key, value string) bool {
	for _, field := range strings.FieldsFunc(out, func(r rune) bool { return r == ',' || r == '\n' }) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key && strings.TrimSpace(v) == value {
			return true
		}
	}
	return false
}

// bridgeCarriesOurMark reports whether this bridge is one kairos-lab built,
// by either of the two marks markBridgeAsOurs puts on it.
func bridgeCarriesOurMark(bridge string) bool {
	if alias, ok := netDeviceIfalias(bridge); ok && alias == bridgeOwnerAlias {
		return true
	}
	data, ok := nmConnectionUserData(bridge)
	return ok && userDataHasItem(data, bridgeOwnerMarkKey, bridgeOwnerMarkValue)
}

// refuseForeignStoredBridge is refuseForeignStoredDevice plus the ownership
// question, for the one name where the kind check is not enough.
//
// The three answers it shares with refuseForeignStoredDevice are unchanged and
// are made in the same order, so the cases that function's doc comment argues
// through still hold: a stat that cannot answer refuses, a name that resolves
// to no device passes -- the leftover profile from an interrupted setup, which
// the stale cleanup exists to remove -- and a device of the wrong kind is
// refused in the words kairos-io/kairos#5051 gave it.
//
// The fourth answer is the new one. A Linux bridge with neither mark on it is
// a bridge this tool did not build, and the commands behind this guard are
// `nmcli connection delete` and `ip link delete`.
//
// A bridge kairos-lab built before this change carries no mark either, and is
// refused with everything else. That is the honest reading -- nothing on the
// host distinguishes it from the host's own br0 -- and the message names the
// two ways out, the first of which is simply to start a VM again, since
// markBridgeAsOurs runs on every start.
func refuseForeignStoredBridge(field, name string) error {
	exists, err := netDeviceExists(name)
	if err != nil {
		return fmt.Errorf("invalid %s %q in stored configuration: cannot tell what kind of device this is: %w", field, name, err)
	}
	if !exists {
		return nil
	}
	if !isLinuxBridge(name) {
		return fmt.Errorf("invalid %s %q in stored configuration: %s is a device on this host and is not a Linux bridge, so this teardown will not delete it", field, name, name)
	}
	if bridgeCarriesOurMark(name) {
		return nil
	}
	return fmt.Errorf("invalid %s %q in stored configuration: %s is a Linux bridge on this host that kairos-lab did not create, so this teardown will not delete it. "+
		"A bridge kairos-lab builds carries the %q device alias and the %s=%s NetworkManager user data, and this one carries neither. "+
		"If kairos-lab built it before this version, `kairos-lab start` marks it and the teardown then accepts it; otherwise remove that name from the state file, or delete the bridge by hand",
		field, name, name, bridgeOwnerAlias, bridgeOwnerMarkKey, bridgeOwnerMarkValue)
}
