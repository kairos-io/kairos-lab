// Tests for the host-networking paths in network_linux.go.
//
// There is deliberately no build tag here. network_linux.go carries none
// either: it compiles only on Linux by virtue of its _linux.go filename, and
// this file's name gives it the identical constraint. So these tests run on
// the ubuntu CI leg and are absent from the macOS one, which is what we want
// -- every symbol they touch exists only on Linux, and the darwin build of
// the package would not compile them.
//
// Almost nothing here goes near the host. sudo, bridgeSlaveLinks and the host
// probes beside them are package-level vars (see the comment above sudo);
// newFakeHost swaps them for an in-process fake that records the argv of
// every command the code would have handed to root, and restores the
// originals with t.Cleanup so no test can leak its fake into another. What
// the fake replaces is only ever a subprocess: findBridgeSlave, which decides
// which interface a teardown reconnects, is a plain function and runs for
// real in every test that reaches it.
//
// "Almost", because one test is a deliberate exception:
// TestTapOwnerUIDHonoursSudoUserFirst runs a real
// `id -u kairoslab-no-such-user` through uidForUser. Faking the resolver
// there would gut the test, whose entire claim is that an unresolvable
// SUDO_USER is an error and not a silent fall back to the current uid, which
// under sudo is root's -- only the real resolver can establish that. The
// subprocess is read-only, changes nothing on the host, and fails the same
// way everywhere, since the account it asks about is one no host has. No
// other test in this file starts a process.
package vm

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/kairos-io/kairos-lab/internal/state"
)

// fakeHost is a host with no NetworkManager, no interfaces and no root. It
// answers the probes from its own maps and applies each recorded command back
// onto them, so a probe made after a command sees what that command did
// rather than a frozen snapshot.
type fakeHost struct {
	conns   map[string]bool
	links   map[string]bool
	bridges map[string]bool
	// slaves lists the interfaces enslaved to a bridge, tap included. A
	// missing key is a bridge with no ports at all; the shared-mode shape is
	// the tap on its own, which findBridgeSlave must report as no slave.
	//
	// The list is in the order `ip -o link show master <bridge>` would print
	// it, and the fake renders real-looking output from it, so the parse and
	// the tap filter in findBridgeSlave run for real. Seeding the tap in the
	// list is the point: a bridge always has its tap on it, and a filter that
	// stopped working would return the tap here.
	slaves map[string][]string
	// profiles is what `nmcli connection add` said about each connection,
	// kept because what a later `connection up` does depends on it: which
	// interface the connection names, whether that interface is a bridge, and
	// which connection it is a port of. A connection seeded straight into
	// conns has no profile, and activating it does nothing -- which is right,
	// since nothing here knows what such a profile would say.
	profiles map[string]nmProfile
	// invisibleLinks are links `ip link show` cannot see although they are
	// on the host. linkExists answers "no" for any failure of that command,
	// which is how a bridge survives a teardown that skipped its `ip link
	// delete` -- and isLinuxBridge, which reads /sys, still reports it. That
	// pair of answers is the state the pre-activation port assertion exists
	// for, and without this field the fake cannot produce it.
	invisibleLinks map[string]bool
	// invisibleBridges are devices the /sys stat cannot be asked about: it
	// fails with something other than "no such file or directory". The stat
	// in question is os.Stat("/sys/class/net/<name>") -- the DEVICE entry,
	// not the "bridge" entry one level under it -- and on a real host the
	// failure that path produces is permission denied, on a /sys/class/net
	// this user cannot read. ENOTDIR would need /sys/class/net itself to be
	// a non-directory, and a symlink loop is not reachable in sysfs;
	// bonding_masters, the usual ENOTDIR example, is a regular file sitting
	// DIRECTLY in /sys/class/net, so only the old <name>/bridge path ever saw
	// ENOTDIR for it. Every one of these makes isLinuxBridge answer false,
	// which is the same answer it gives for a device that is not there -- and
	// that is the pair netDeviceExists exists to tell apart. The value is the
	// error the stat failed with, because which error it is decides the
	// answer.
	invisibleBridges map[string]error
	// notRealTapDevices names a port that parses as a generated tap name
	// (bridgePortExempt's condition 1) but is not actually a tun/tap device --
	// modelling an attacker who named a real NIC "kairoslab-tap5".
	// bridgePortExempt's condition 2 must answer false for it, so it is
	// never exempt from refusal however it is named.
	notRealTapDevices map[string]bool
	// foreignOwnedTaps names a generated, real tap device (conditions 1 and
	// 2 both hold) whose owner is not this run's own uid -- condition 3
	// failing on its own. Absent from this map, a generated name that
	// passes condition 1 is modelled as a real, self-owned tap device by
	// default, which is what an ordinary kairos-lab-created tap is.
	foreignOwnedTaps map[string]bool
	// slaveLinksCalls counts how many times the port list was read. The
	// number is a claim internal/app's consent paragraph makes out loud, so
	// it is pinned rather than described.
	slaveLinksCalls int
	// slaveLinksErr, when set, is what the `ip -o link show master` probe
	// fails with. A busybox `ip` has no `show master` filter and exits 2 for
	// every bridge on the host, so this models a property of the machine
	// rather than one bad call.
	slaveLinksErr error
	// failCmd, when set, decides which recorded commands fail. A failing
	// command is still recorded but is not applied to the host state, which
	// is how a partial teardown really behaves.
	failCmd func(argv []string) error
	// onCommand, when set, runs after a successful command has been applied,
	// and is how a test models a consequence the fake cannot know about on
	// its own. The one that matters here is NetworkManager enslaving an
	// interface the instant `connection up <bridge>` activates the
	// controller, because some profile elsewhere on the host carries
	// `master <bridge> slave-type bridge` and autoconnect: the fake has no
	// model of foreign profiles, so the test supplies the effect.
	onCommand func(h *fakeHost, argv []string)
	commands  [][]string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{
		conns:             map[string]bool{},
		links:             map[string]bool{},
		bridges:           map[string]bool{},
		slaves:            map[string][]string{},
		profiles:          map[string]nmProfile{},
		invisibleLinks:    map[string]bool{},
		invisibleBridges:  map[string]error{},
		notRealTapDevices: map[string]bool{},
		foreignOwnedTaps:  map[string]bool{},
	}

	origSudo := sudo
	origNMActive := networkManagerActive
	origNmcli := nmcliAvailable
	origConnExists := nmConnectionExists
	origIsBridge := isLinuxBridge
	origStatDevice := statNetDevice
	origLinkExists := linkExists
	origSlaveLinks := bridgeSlaveLinks
	origDelay := staleCleanupSettleDelay
	origTunFlags := tapSysfsTunFlagsReadable
	origOwnerUID := tapSysfsOwnerUID
	t.Cleanup(func() {
		sudo = origSudo
		networkManagerActive = origNMActive
		nmcliAvailable = origNmcli
		nmConnectionExists = origConnExists
		isLinuxBridge = origIsBridge
		statNetDevice = origStatDevice
		linkExists = origLinkExists
		bridgeSlaveLinks = origSlaveLinks
		staleCleanupSettleDelay = origDelay
		tapSysfsTunFlagsReadable = origTunFlags
		tapSysfsOwnerUID = origOwnerUID
	})

	sudo = h.run
	networkManagerActive = func() bool { return true }
	nmcliAvailable = func() bool { return true }
	nmConnectionExists = func(name string) bool { return name != "" && h.conns[name] }
	// A failing stat is a false here, whatever it failed with: the real
	// isLinuxBridge is `os.Stat(...); return err == nil`, so an unreadable
	// /sys and a missing device are one answer to it.
	isLinuxBridge = func(name string) bool {
		return name != "" && h.invisibleBridges[name] == nil && h.bridges[name]
	}
	// The same stat, rendered as os.Stat renders it: nil for a device that is
	// there -- whether or not it is a bridge, which is what a bond master
	// looks like under /sys/class/net/<name> -- a *fs.PathError wrapping
	// fs.ErrNotExist for one that is not, and whatever invisibleBridges says
	// when the stat itself cannot answer. Only the raw result is faked; the
	// classification of it is netDeviceExists's and runs for real here.
	statNetDevice = func(path string) error {
		// The seam is handed a finished path, so the fake maps it back to
		// the name its tables are keyed by -- and says so loudly when it
		// cannot. Anything other than one entry directly under
		// /sys/class/net means netDevicePath stopped asking "is a device of
		// this name on the host", which is the question the refusal is built
		// on; without this the suite went green over exactly that change.
		name := strings.TrimPrefix(path, sysClassNet+"/")
		if name == path || name == "" || strings.Contains(name, "/") {
			t.Errorf("the device stat was handed %q, which is not one device's entry under %s", path, sysClassNet)
			return &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
		}
		if err := h.invisibleBridges[name]; err != nil {
			return err
		}
		if h.links[name] || h.bridges[name] {
			return nil
		}
		return &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	linkExists = func(name string) bool {
		return name != "" && !h.invisibleLinks[name] && (h.links[name] || h.bridges[name])
	}
	bridgeSlaveLinks = h.slaveLinks
	staleCleanupSettleDelay = 0
	// bridgePortExempt's two /sys reads. A name that parses as generated
	// (condition 1) is modelled as a real, self-owned tap device by default --
	// an ordinary kairos-lab-created tap -- unless the test opts a name into
	// notRealTapDevices (condition 2 fails: not a tun/tap device at all) or
	// foreignOwnedTaps (condition 3 fails: a real generated-name tap device
	// this run did not create).
	tapSysfsTunFlagsReadable = func(name string) bool {
		if h.notRealTapDevices[name] {
			return false
		}
		_, ok := IsGeneratedTapName(name)
		return ok
	}
	tapSysfsOwnerUID = func(name string) (string, bool) {
		if h.foreignOwnedTaps[name] {
			return "not-this-uid", true
		}
		want, err := tapOwnerUID()
		if err != nil {
			return "", false
		}
		return want, true
	}

	// Both variables empty keeps tapOwnerUID on its os.Getuid() branch, so the
	// uid in the expected argv is known and no subprocess runs.
	t.Setenv("SUDO_USER", "")
	t.Setenv("USER", "")
	return h
}

func (h *fakeHost) run(name string, args ...string) error {
	argv := append([]string{name}, args...)
	h.commands = append(h.commands, argv)
	if h.failCmd != nil {
		if err := h.failCmd(argv); err != nil {
			// Wrapped the way the real sudo wraps it, and not returned
			// bare. What failCmd supplies is the subprocess's failure --
			// `exit status 1` -- and what a caller of sudo actually
			// receives is that failure inside sudoError's message, which
			// carries a SECOND copy of the argv. A fake that skipped the
			// wrapper made every test that reads a teardown error measure
			// a message no user is ever shown, and hid the copy of the
			// interface name that message puts on the terminal.
			//
			// Not applied: a command that failed changed nothing.
			return sudoError(argv, err)
		}
	}
	h.apply(argv)
	if h.onCommand != nil {
		h.onCommand(h, argv)
	}
	return nil
}

// slaveLinks renders `ip -o link show master <bridge>` from the fake's own
// slave list, one line per attached interface, in the kernel's format -- or
// fails the way a host that cannot answer the question does.
func (h *fakeHost) slaveLinks(bridge string) (string, error) {
	h.slaveLinksCalls++
	if h.slaveLinksErr != nil {
		return "", h.slaveLinksErr
	}
	var b strings.Builder
	for i, iface := range h.slaves[bridge] {
		b.WriteString(ipLinkLine(i+3, iface, bridge))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// nmProfile is the part of a NetworkManager connection this fake models: the
// device it names, whether that device is a bridge, and the connection it is
// a port of.
type nmProfile struct {
	ifname   string
	master   string
	isBridge bool
}

// activate is what `nmcli connection up <conn>` does to the host, and what
// NetworkManager does by itself to a connection added with autoconnect yes:
// the device appears, and a connection with a master becomes a port of that
// master's device.
//
// Having the fake do this is what makes the shared path's port assertions
// mean anything. The tap becomes a port of the bridge when the TAP
// connection is activated, which is the last command of the shared sequence
// -- so at the two checks before it the bridge has no ports at all, and at
// the one after it the tap is its only one. A fake that attached the tap
// earlier, or at `connection add`, would be asserting against an ordering no
// host has.
func (h *fakeHost) activate(conn string) {
	p, ok := h.profiles[conn]
	if !ok || p.ifname == "" {
		return
	}
	h.links[p.ifname] = true
	if p.isBridge {
		h.bridges[p.ifname] = true
	}
	master, ok := h.profiles[p.master]
	if !ok || master.ifname == "" {
		return
	}
	if !slices.Contains(h.slaves[master.ifname], p.ifname) {
		h.slaves[master.ifname] = append(h.slaves[master.ifname], p.ifname)
	}
}

// deactivate is `nmcli connection down <conn>`, and the half of `nmcli
// connection delete <conn>` that is not the deletion: NetworkManager takes
// the device it created away with the connection, and its ports with it.
func (h *fakeHost) deactivate(conn string) {
	p, ok := h.profiles[conn]
	if !ok || p.ifname == "" {
		return
	}
	delete(h.links, p.ifname)
	delete(h.bridges, p.ifname)
	delete(h.slaves, p.ifname)
	if master, ok := h.profiles[p.master]; ok && master.ifname != "" {
		h.slaves[master.ifname] = slices.DeleteFunc(h.slaves[master.ifname], func(port string) bool {
			return port == p.ifname
		})
	}
}

func (h *fakeHost) apply(argv []string) {
	if len(argv) < 4 {
		return
	}
	switch {
	case argv[0] == "nmcli" && argv[1] == "connection" && argv[2] == "delete":
		h.deactivate(argv[3])
		delete(h.conns, argv[3])
		delete(h.profiles, argv[3])
	case argv[0] == "nmcli" && argv[1] == "connection" && argv[2] == "down":
		h.deactivate(argv[3])
	case argv[0] == "nmcli" && argv[1] == "connection" && argv[2] == "up":
		h.activate(argv[3])
	case argv[0] == "ip" && argv[1] == "link" && argv[2] == "delete":
		delete(h.links, argv[3])
		delete(h.bridges, argv[3])
		delete(h.slaves, argv[3])
	case argv[0] == "nmcli" && argv[1] == "connection" && argv[2] == "add":
		conn := valueAfter(argv, "con-name")
		if conn == "" {
			return
		}
		h.conns[conn] = true
		h.profiles[conn] = nmProfile{
			ifname:   valueAfter(argv, "ifname"),
			master:   valueAfter(argv, "master"),
			isBridge: valueAfter(argv, "type") == "bridge",
		}
		// NetworkManager brings up a connection it has just been given with
		// autoconnect yes, which is what creates the device. Both of the
		// shared path's connections carry autoconnect no, so on that path
		// nothing exists until the explicit `connection up`.
		if valueAfter(argv, "autoconnect") == "yes" {
			h.activate(conn)
		}
	}
}

// lines renders the recorded commands one per line, in order, so a whole
// sequence can be pinned as data instead of as a chain of assertions.
func (h *fakeHost) lines() []string {
	out := make([]string, 0, len(h.commands))
	for _, argv := range h.commands {
		out = append(out, strings.Join(argv, " "))
	}
	return out
}

func valueAfter(argv []string, key string) string {
	for i, a := range argv {
		if a == key && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func assertSequence(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("issued %d commands, want %d:\n got:\n  %s\nwant:\n  %s",
			len(got), len(want), strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d differs:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

func testUID(t *testing.T) string {
	t.Helper()
	return strconv.Itoa(os.Getuid())
}

// sharedSequence is the argv sequence --network shared produces on a host
// with nothing of ours on it. It is spelled out rather than generated: the
// point of pinning it is that a change to the real sequence has to be
// retyped here by whoever makes it.
func sharedSequence(uid string) []string {
	return []string{
		"nmcli connection add type bridge ifname kairoslab0 con-name kairoslab0 autoconnect no stp no",
		"nmcli connection modify kairoslab0 connection.interface-name kairoslab0 ipv4.method shared ipv6.method ignore bridge.stp no connection.autoconnect no",
		"nmcli connection add type tun ifname kairoslab-tap0 con-name kairoslab0-tap mode tap owner " + uid + " master kairoslab0 slave-type bridge autoconnect no",
		"nmcli connection modify kairoslab0-tap connection.interface-name kairoslab-tap0 tun.mode tap tun.owner " + uid + " master kairoslab0 slave-type bridge connection.autoconnect no",
		"nmcli connection up kairoslab0",
		"nmcli connection up kairoslab0-tap",
	}
}

// bridgedSequence is the same for --network bridged over eth0. It is pinned
// so the shared work cannot quietly alter shipped behaviour: every line here
// predates it.
func bridgedSequence(uid string) []string {
	return []string{
		"nmcli connection add type bridge ifname kairoslab0 con-name kairoslab0 autoconnect yes stp no",
		"nmcli connection modify kairoslab0 connection.interface-name kairoslab0 ipv4.method auto ipv6.method auto bridge.stp no connection.autoconnect yes",
		"nmcli connection add type ethernet ifname eth0 con-name kairoslab0-uplink master kairoslab0 slave-type bridge autoconnect yes",
		"nmcli connection modify kairoslab0-uplink connection.interface-name eth0 master kairoslab0 slave-type bridge connection.autoconnect yes",
		"nmcli connection add type tun ifname kairoslab-tap0 con-name kairoslab0-tap mode tap owner " + uid + " master kairoslab0 slave-type bridge autoconnect yes",
		"nmcli connection modify kairoslab0-tap connection.interface-name kairoslab-tap0 tun.mode tap tun.owner " + uid + " master kairoslab0 slave-type bridge connection.autoconnect yes",
		"nmcli connection up kairoslab0",
		"nmcli connection up kairoslab0-uplink",
		"nmcli connection up kairoslab0-tap",
	}
}

func TestPrepareLinuxSharedCleanHostSequence(t *testing.T) {
	h := newFakeHost(t)
	st := &state.State{}

	if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxShared: %v", err)
	}

	assertSequence(t, h.lines(), sharedSequence(testUID(t)))

	// The mode's defining property, asserted separately from the sequence so
	// it survives a rewrite of the expected lines: nothing in shared mode
	// ever names an uplink connection, because nothing enslaves a host NIC.
	for _, line := range h.lines() {
		if strings.Contains(line, "-uplink") {
			t.Errorf("shared path names an uplink connection: %s", line)
		}
	}
}

// Regression test for the orphaned <bridge>-uplink connection.
//
// cleanupNMConnections attempts every delete and reports the ones that failed
// rather than stopping at the first, so a host can be left with the bridge
// connection gone and <bridge>-uplink still present. The preflight used to
// ask only about the bridge link and the bridge connection, so it saw that
// host as clean -- and the orphan, which carries master/slave-type bridge and
// autoconnect, would have enslaved the physical NIC to the NAT bridge the
// shared path then brought up. Seeing it is half the fix; what happens when
// the delete that follows FAILS is the other half, one test below.
func TestPrepareLinuxSharedDeletesOrphanedUplinkConnection(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName+"-uplink"] = true
	st := &state.State{}

	if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxShared: %v", err)
	}

	want := append([]string{"nmcli connection delete kairoslab0-uplink"}, sharedSequence(testUID(t))...)
	assertSequence(t, h.lines(), want)

	if h.conns[DefaultBridgeName+"-uplink"] {
		t.Errorf("the orphaned uplink connection is still on the host after PrepareLinuxShared")
	}
}

// The same orphan, this time one that will not go.
//
// Seeing a stale <bridge>-uplink connection and failing to delete it used to
// end the same way as seeing nothing at all: the preflight discarded the
// cleanup error, prepareLinuxSharedWithNM built the NAT bridge over the top
// and brought it up, and NetworkManager enslaved the host's physical NIC to a
// bridge carrying ipv4.method shared -- because that is what the orphan says
// to do. The run succeeded, the consent prompt above it had promised "no
// uplink interface is used", and the host lost its connectivity.
//
// So the delete failing is fatal for shared. Nothing may be issued after it,
// and the error has to name the leftover and a way out, since the user is the
// only one who can clear it.
func TestPrepareLinuxSharedRefusesWhenAStaleUplinkWillNotGo(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName+"-uplink"] = true
	h.failCmd = func(argv []string) error {
		if strings.Join(argv, " ") == "nmcli connection delete kairoslab0-uplink" {
			return fmt.Errorf("exit status 1")
		}
		return nil
	}
	st := &state.State{}

	err := PrepareLinuxShared(st, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared built a NAT bridge over an uplink connection it could not delete")
	}

	// The refusal is the whole of the run: the failed delete is the last
	// thing issued, so no bridge was created, modified or brought up.
	assertSequence(t, h.lines(), []string{"nmcli connection delete kairoslab0-uplink"})
	if h.bridges[DefaultBridgeName] || h.links[DefaultTapName] {
		t.Errorf("the host was changed by a refused run: bridge=%v tap=%v",
			h.bridges[DefaultBridgeName], h.links[DefaultTapName])
	}

	// What is still on the host, and what to do about it. The wrapped error
	// from cleanupNMConnections names the step that failed -- which is where
	// the connection name comes from, and the only place it may come from:
	// the refusal used to spell out a "-uplink" connection of its own, in
	// states where none exists. See the test below.
	for _, want := range []string{
		"delete connection kairoslab0-uplink",
		"Remove the leftover the failure above names",
		"-network bridged",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}

	if st.Network.Mode != "" || st.Network.BridgeName != "" || st.Network.CreatedByKairosLab {
		t.Errorf("state was written for a run that prepared nothing: %+v", st.Network)
	}
}

// The refusal above is charged on a cleanup that FAILED, which is not the
// same fact as "a host interface is enslaved to this bridge", and the message
// may not claim it is.
//
// Both cases below are states the old wording was simply false in. It said "a
// surviving <bridge>-uplink connection carries master <bridge> slave-type
// bridge, and NetworkManager would enslave the host's own NIC to that bridge",
// and told the user to run `nmcli connection delete <bridge>-uplink`: after a
// previous SHARED run there is no -uplink connection on the host at all, and
// the joined error also covers the `nmcli device connect` reconnect, where
// every delete succeeded and nothing is enslaved to anything.
//
// The message has to carry the other half too. cleanupNMConnections does not
// stop at its first failure, so by the time this refusal returns the deletes,
// the `ip link delete`s and the reconnect have all run, and the host can be
// sitting there with its NIC disconnected.
func TestPrepareLinuxSharedRefusalDescribesOnlyWhatItKnows(t *testing.T) {
	// The state a second shared run arrives in: the bridge connection and the
	// bridge link from the run before it, and no -uplink connection anywhere,
	// because the shared path never creates one.
	t.Run("no uplink connection exists", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-tap"] = true
		h.bridges[DefaultBridgeName] = true
		h.links[DefaultTapName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName}
		h.failCmd = func(argv []string) error {
			if strings.Join(argv, " ") == "nmcli connection delete kairoslab0" {
				return fmt.Errorf("exit status 1")
			}
			return nil
		}

		err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false)
		if err == nil {
			t.Fatal("PrepareLinuxShared continued over a stale bridge connection it could not delete")
		}
		assertRefusalWording(t, err)
	})

	// Every delete succeeded and the host NIC was released; the one step that
	// failed is the reconnect that was putting it back. Nothing is enslaved
	// to this bridge -- and the user has a disconnected NIC to hear about.
	//
	// The -uplink connection is seeded because the reconnect is only issued
	// once this teardown has deleted it: the state under test here is a
	// bridged leftover that came away cleanly, with the reconnect behind it
	// the single step that failed.
	t.Run("the failure is the reconnect step", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-uplink"] = true
		h.bridges[DefaultBridgeName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName, "eth0"}
		h.failCmd = func(argv []string) error {
			if strings.Join(argv, " ") == "nmcli device connect eth0" {
				return fmt.Errorf("exit status 1")
			}
			return nil
		}

		err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false)
		if err == nil {
			t.Fatal("PrepareLinuxShared continued over a teardown that could not finish")
		}
		if !strings.Contains(err.Error(), `reconnect "eth0"`) {
			t.Errorf("the refusal does not name the step that failed:\n%v", err)
		}
		assertRefusalWording(t, err)
	})
}

// The same refusal, held to what the cleanup it is charged on actually
// issued.
//
// It used to say "every step after the one above was still attempted:
// deleting the remaining <bridge> connections, deleting the <bridge> and
// <tap> interfaces, and handing any host interface that was enslaved to the
// bridge to `nmcli device connect`" -- a list of steps, presented as things
// that had happened. Two of those are gated on linkExists and the third on an
// interface having been found on the bridge. The state below is the ordinary
// one after a reboot: the connection keyfile survives, the interfaces do not,
// and the single `nmcli connection delete` is the only command the whole
// teardown issues. The message claimed the interface deletions were attempted
// on a host where nothing of the sort was.
func TestPrepareLinuxSharedCleanupRefusalClaimsOnlyWhatWasIssued(t *testing.T) {
	h := newFakeHost(t)
	// The keyfile outlived the reboot; the bridge and the tap did not.
	h.conns[DefaultBridgeName] = true
	h.failCmd = func(argv []string) error {
		if strings.Join(argv, " ") == "nmcli connection delete kairoslab0" {
			return fmt.Errorf("exit status 1")
		}
		return nil
	}

	err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared continued over a stale connection it could not delete")
	}
	// One command for the whole run, which is what makes the old wording
	// false rather than merely imprecise.
	assertSequence(t, h.lines(), []string{"nmcli connection delete kairoslab0"})

	msg := err.Error()
	if strings.Contains(msg, "deleting the kairoslab0 and kairoslab-tap0 interfaces") {
		t.Errorf("the refusal claims interface deletions that were never issued:\n%v", err)
	}
	for _, want := range []string{
		"where it had anything to attempt",
		"`ip link show` can see",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	assertRefusalWording(t, err)
}

// assertRefusalWording holds the preflight refusal to what it actually knows:
// a teardown step failed, and the rest of the teardown ran anyway.
func assertRefusalWording(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), "-uplink") {
		t.Errorf("the refusal names a -uplink connection in a state that has none:\n%v", err)
	}
	// What the partial teardown already did to this host, which is the half
	// the user cannot see from the failure alone.
	for _, want := range []string{
		"still attempted",
		"nmcli device connect",
		"-network bridged",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

// The invariant shared mode exists for is "no host interface is a port of
// this bridge", and these three tests are where it is enforced: against the
// kernel's port list, either side of bringing the bridge up.
//
// Charging the refusal on a failed cleanup instead let two hosts through. One:
// nmConnectionExists runs `nmcli connection show <name>` and reads ANY
// non-zero exit as "does not exist", so a profile it cannot see is never
// deleted, cleanupNMConnections joins no errors and returns nil, and the
// preflight declares the host clean. Two: cleanupNMConnections only ever
// touches <bridge>-tap, <bridge>-uplink and <bridge>, so a profile with any
// other name -- a "Wired connection 1" somebody pointed at this bridge --
// survives a cleanup that fully succeeded. In both, the bridge comes up
// carrying ipv4.method shared, the profile enslaves the host's NIC to it, and
// the host's connectivity goes behind a NAT bridge.
//
// The fake host models exactly that: no connection of ours is visible to any
// probe, and the interface appears on the bridge when the bridge activates.
func TestPrepareLinuxSharedRefusesAnInterfaceEnslavedWhenTheBridgeComesUp(t *testing.T) {
	// enslaveOnBridgeUp is the foreign profile's effect, and the only part of
	// it the host can observe: eth0 becomes a port of the bridge when the
	// bridge activates. Whatever the profile is called, this is what arrives.
	//
	// eth0 alone, with no tap beside it: the tap connection is brought up
	// after this check, so at this point the bridge is meant to have no ports
	// at all and eth0 is the whole of the list.
	enslaveOnBridgeUp := func(h *fakeHost, argv []string) {
		if strings.Join(argv, " ") == "nmcli connection up kairoslab0" {
			h.slaves[DefaultBridgeName] = []string{"eth0"}
		}
	}

	tests := []struct {
		name string
		seed func(h *fakeHost)
		// prefix is what the preflight issues before the shared sequence.
		prefix []string
	}{
		{
			// nmConnectionExists cannot see the profile, so the preflight
			// finds nothing stale, deletes nothing, joins no errors and
			// reports a clean host. The old refusal, charged on that error,
			// never fired.
			name: "the connection probes are blind to the profile",
			seed: func(h *fakeHost) {},
		},
		{
			// Nothing is blind here and nothing fails: the leftover this
			// cleanup knows about is deleted, and it returns nil. The
			// profile that enslaves eth0 is named something
			// cleanupNMConnections never touches, so a fully successful
			// teardown leaves it exactly where it was.
			name: "cleanup fully succeeds with a foreign profile present",
			seed: func(h *fakeHost) {
				h.conns[DefaultBridgeName+"-tap"] = true
				h.conns["Wired connection 1"] = true
			},
			prefix: []string{"nmcli connection delete kairoslab0-tap"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			tt.seed(h)
			h.onCommand = enslaveOnBridgeUp
			st := &state.State{}

			err := PrepareLinuxShared(st, t.TempDir(), 0, false)
			if err == nil {
				t.Fatal("PrepareLinuxShared left the host's NIC enslaved to a NAT bridge and reported success")
			}

			// The refusal names the interface the KERNEL reported, because
			// that is the only name anything here actually knows: the
			// profile that enslaved it may be called anything at all.
			if !strings.Contains(err.Error(), "eth0") {
				t.Errorf("the refusal does not name the interface found on the bridge:\n%v", err)
			}
			// And the bridge goes back down and the connections this start
			// wrote are deleted, so the refusal releases the NIC instead of
			// leaving it on a bridge carrying ipv4.method shared, and does
			// not leave that method persisted in a keyfile behind it. The tap
			// is never brought up: no guest is put on a bridge this run is
			// abandoning.
			uid := testUID(t)
			shared := sharedSequence(uid)
			want := append([]string{}, tt.prefix...)
			want = append(want, shared[:len(shared)-1]...)
			want = append(want, refusalTail()...)
			assertSequence(t, h.lines(), want)

			if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
				t.Errorf("state was written for a run that was refused: %+v", st.Network)
			}
		})
	}
}

// The same assertion at the other end: an interface already on the bridge
// before anything is activated.
//
// The state is the one this check is written for, and both halves of it
// matter. isLinuxBridge reads /sys and says a bridge is there; linkExists
// reads `ip link show` and answers "no" for any failure of it, so the
// teardown that ran a moment earlier skipped the `ip link delete` that would
// have removed the bridge, reported nothing wrong, and left eth0 on it. The
// profile this path has just re-pointed at ipv4.method shared is the one
// about to be applied to that bridge.
func TestPrepareLinuxSharedRefusesAnInterfaceAlreadyEnslavedBeforeTheBridgeComesUp(t *testing.T) {
	h := newFakeHost(t)
	h.bridges[DefaultBridgeName] = true
	h.invisibleLinks[DefaultBridgeName] = true
	h.slaves[DefaultBridgeName] = []string{"eth0"}
	st := &state.State{}

	err := PrepareLinuxShared(st, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared brought a NAT bridge up over an enslaved host NIC")
	}
	if !strings.Contains(err.Error(), `"eth0"`) {
		t.Errorf("the refusal does not name the interface found on the bridge:\n%v", err)
	}

	// The teardown the preflight ran could see eth0 on the bridge and nothing
	// else: not the bridge device, which linkExists is blind to here, and no
	// connection of ours, since none is seeded. So it issued no command at
	// all -- in particular not `nmcli device connect eth0`, which would hand
	// eth0 to whichever profile NetworkManager rates best for it while every
	// profile this teardown knows about was left untouched. The bridge is
	// still there with eth0 on it when the assertion asks.
	uid := testUID(t)
	want := append([]string{}, sharedSequence(uid)[:4]...)
	want = append(want, refusalTail()...)
	assertSequence(t, h.lines(), want)

	// Nothing is activated at all: the check sits before the `connection up`,
	// so ipv4.method shared is never applied to a bridge carrying a NIC.
	for _, line := range h.lines() {
		if strings.HasPrefix(line, "nmcli connection up") {
			t.Errorf("a connection was activated over an enslaved host NIC: %s", line)
		}
	}
	if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
		t.Errorf("state was written for a run that was refused: %+v", st.Network)
	}
}

// refusalTail is what a refused shared start issues after the check fires.
//
// The `down` releases whatever was attached. The two deletes are the rest of
// the revert, and they are the half a refusal used to skip: `nmcli connection
// modify <bridge> ... ipv4.method shared` has already returned by the time
// any of these checks runs, so a refusal that only deactivated would leave a
// persisted NAT-bridge profile on the host -- next to the very profile that
// attaches an interface to that bridge, which ordinarily carries autoconnect
// yes. See revertSharedSetup.
func refusalTail() []string {
	return []string{
		"nmcli connection down " + DefaultBridgeName,
		"nmcli connection delete " + DefaultBridgeName + "-tap",
		"nmcli connection delete " + DefaultBridgeName,
	}
}

// The clean case, which is the one that has to stay silent: the tap is a port
// of this bridge on every successful run, and it is not a foreign one. A
// check that could not tell them apart would refuse every shared start and
// take the bridge down behind it.
//
// Nothing stages the tap here. The fake attaches it where a host does, when
// the tap connection is activated, which is after the two checks that expect
// no ports and before the one that expects exactly this.
func TestPrepareLinuxSharedAcceptsABridgeWhoseOnlyPortIsTheTap(t *testing.T) {
	h := newFakeHost(t)
	st := &state.State{}

	if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxShared refused a bridge whose only port is its own tap: %v", err)
	}
	// The sequence is the ordinary one, so the assertions issued no command
	// of their own -- a `connection down` here would deactivate the bridge
	// the run just built.
	assertSequence(t, h.lines(), sharedSequence(testUID(t)))
	if got := h.slaves[DefaultBridgeName]; len(got) != 1 || got[0] != DefaultTapName {
		t.Fatalf("the bridge ended with ports %v, want just the tap -- the fixture, not the code, is what this test would be proving", got)
	}
	if st.Network.Mode != "shared" {
		t.Errorf("Mode = %q, want %q", st.Network.Mode, "shared")
	}
}

// The escape that made the port list the only thing enforcing the invariant
// worth nothing on whole classes of host.
//
// `ip -o link show master <bridge>` is not a command every `ip` has. Busybox's
// does not take `show master` at all -- it exits 2 with `either "dev" is
// duplicate, or "br0" is garbage` -- and neither does an iproute2 older than
// the filter, and neither does a host with no `ip` on PATH. The probe used to
// answer "" for every one of those, "" is what a bridge with no ports also
// answers, and that means PROCEED. So on a busybox host the check passed over
// a real bridge with a real host NIC on it, permanently, and the start
// completed and wrote its state.
func TestPrepareLinuxSharedRefusesWhenThePortListCannotBeRead(t *testing.T) {
	h := newFakeHost(t)
	// The same host as the test above -- a leftover bridge with eth0 on it --
	// except that nothing here can read the port list.
	h.bridges[DefaultBridgeName] = true
	h.invisibleLinks[DefaultBridgeName] = true
	h.slaves[DefaultBridgeName] = []string{"eth0"}
	h.slaveLinksErr = fmt.Errorf(`exit status 2: %q`, `ip: either "dev" is duplicate, or "kairoslab0" is garbage`)
	st := &state.State{}

	err := PrepareLinuxShared(st, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared built a NAT bridge on a host where it could not read the bridge's port list")
	}

	// The refusal has to be about the check, not about a port: nothing was
	// seen on the bridge, and the message may not say anything was. What it
	// owes the user is the reason the check could not run and a way to look
	// for themselves.
	for _, want := range []string{
		"could not be run",
		"ip -o link show master kairoslab0",
		"is garbage",
		"busybox",
		"ip -V",
		"/sys/class/net/kairoslab0/brif",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "reports") {
		t.Errorf("the refusal claims a port list it never read:\n%v", err)
	}

	// No `nmcli device connect eth0` anywhere in here: the teardown's
	// reconnect hint reads the same probe and answers "" when it fails, which
	// is the tolerance findBridgeSlave keeps on purpose. A teardown that
	// refused to run because `ip` could not answer would leave the bridge and
	// its profiles standing instead of deleting them.
	uid := testUID(t)
	want := append([]string{}, sharedSequence(uid)[:4]...)
	want = append(want, refusalTail()...)
	assertSequence(t, h.lines(), want)
	if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
		t.Errorf("state was written for a run that was refused: %+v", st.Network)
	}
}

// The cost of failing closed, stated as a test so it is a decision and not a
// surprise: a host that cannot answer the question is refused even when
// nothing at all is wrong with it.
//
// There is no leftover here and no foreign port -- the bridge this start
// builds is its own, and the check that refuses is the one after `connection
// up`, where the bridge exists and its port list still cannot be read. Shared
// mode's promise is that no host interface is a port of its bridge, and this
// start cannot keep a promise it cannot check. The message is the whole of
// what such a user gets, which is why the test above pins it.
func TestPrepareLinuxSharedRefusesACleanHostThatCannotAnswer(t *testing.T) {
	h := newFakeHost(t)
	h.slaveLinksErr = fmt.Errorf("exec: \"ip\": executable file not found in $PATH")
	st := &state.State{}

	err := PrepareLinuxShared(st, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared completed without ever reading the bridge's port list")
	}
	if !strings.Contains(err.Error(), "could not be run") {
		t.Errorf("the refusal does not say the check could not run:\n%v", err)
	}

	// The bridge is brought up and taken back down again: the first check
	// passes because no bridge of that name exists yet, and the second one is
	// where the unreadable list is fatal. The tap is never activated.
	uid := testUID(t)
	shared := sharedSequence(uid)
	want := append([]string{}, shared[:len(shared)-1]...)
	want = append(want, refusalTail()...)
	assertSequence(t, h.lines(), want)
}

// The other escape: the one name the check was told to ignore was named by
// the file the check exists to defend against.
//
// st.Network.TapName comes out of state.json, a 0644 file anything running as
// the user can write, and validateStoredInterfaceName accepts "eth0" there.
// The old assertion excluded that name from the port list, so with TapName
// set to the host's own NIC both checks passed with eth0 as the bridge's only
// port, the start completed, and state was written with mode "shared".
//
// Expecting NO ports before the tap is activated is what closes it, and it
// closes it without knowing anything about the attack: there is no name to
// exempt, so there is nothing to aim at the NIC.
func TestPrepareLinuxSharedRefusesAHostNICStoredAsTheTapName(t *testing.T) {
	// Both checks that run before the tap is activated, one per subtest,
	// since the escape was through both of them.
	t.Run("already on the bridge", func(t *testing.T) {
		h := newFakeHost(t)
		h.bridges[DefaultBridgeName] = true
		h.invisibleLinks[DefaultBridgeName] = true
		h.slaves[DefaultBridgeName] = []string{"eth0"}
		st := &state.State{}
		st.Network.TapName = "eth0"

		err := PrepareLinuxShared(st, t.TempDir(), 0, false)
		if err == nil {
			t.Fatal("PrepareLinuxShared accepted the host's own NIC as the bridge's port because state.json called it the tap")
		}
		if !strings.Contains(err.Error(), `"eth0"`) {
			t.Errorf("the refusal does not name the port it found:\n%v", err)
		}
		if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
			t.Errorf("state was written for a run that was refused: %+v", st.Network)
		}
	})

	t.Run("attached when the bridge comes up", func(t *testing.T) {
		h := newFakeHost(t)
		h.onCommand = func(h *fakeHost, argv []string) {
			if strings.Join(argv, " ") == "nmcli connection up kairoslab0" {
				h.slaves[DefaultBridgeName] = []string{"eth0"}
			}
		}
		st := &state.State{}
		st.Network.TapName = "eth0"

		err := PrepareLinuxShared(st, t.TempDir(), 0, false)
		if err == nil {
			t.Fatal("PrepareLinuxShared let a foreign profile attach the host NIC because state.json called it the tap")
		}
		if !strings.Contains(err.Error(), `"eth0"`) {
			t.Errorf("the refusal does not name the port it found:\n%v", err)
		}
		for _, line := range h.lines() {
			if line == "nmcli connection up kairoslab0-tap" {
				t.Error("the tap was activated over a bridge carrying the host's NIC")
			}
		}
	})
}

// The blind spot beside the one above: the DEFAULT tap name used to be
// excluded from the port list unconditionally, whatever tap the run was
// configured with.
//
// It was excluded so that a tap name edited in state.json after the tap was
// created could not make the old tap look like a physical slave -- a
// teardown's concern, and parseBridgeSlave still has it for that reason. Here
// it meant that on a run configured with any other tap name, an interface
// called kairoslab-tap0 was a free pass onto the bridge. Expecting no ports
// before the tap is activated leaves no name exempt, this one included.
func TestPrepareLinuxSharedRefusesAPortNamedLikeTheDefaultTap(t *testing.T) {
	h := newFakeHost(t)
	// Merely parsing as a generated tap name is not enough to be exempt: it
	// also has to be a real tap device (bridgePortExempt's condition 2),
	// which this one is modelled as not being -- a foreign NIC coincidentally
	// named like the generated default, the scenario this test is about. Tap
	// identity is index-derived (M2) and no longer read from
	// st.Network.TapName at all, so there is no state field left to set to
	// keep this run's own tap from being the default one.
	h.notRealTapDevices[DefaultTapName] = true
	h.onCommand = func(h *fakeHost, argv []string) {
		if strings.Join(argv, " ") == "nmcli connection up kairoslab0" {
			h.slaves[DefaultBridgeName] = []string{DefaultTapName}
		}
	}
	st := &state.State{}

	err := PrepareLinuxShared(st, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared accepted a port on the bridge because it was named like the default tap")
	}
	if !strings.Contains(err.Error(), `"`+DefaultTapName+`"`) {
		t.Errorf("the refusal does not name the port it found:\n%v", err)
	}
	for _, line := range h.lines() {
		if line == "nmcli connection up kairoslab0-tap" {
			t.Error("the tap was activated over a bridge with a port this run did not put there")
		}
	}
}

// The third assertion, after the tap is up, where the expected port list is
// exactly the tap.
//
// It costs one `ip` call and it narrows the window the check before it leaves
// open: `nmcli connection up` returns when the connection has activated, and
// an interface attached in the instant after that is not seen until the next
// start. Here the foreign profile wins the race with the tap's own
// activation, which is the only part of that window this can close.
func TestPrepareLinuxSharedRefusesAnInterfaceThatArrivesWithTheTap(t *testing.T) {
	h := newFakeHost(t)
	h.onCommand = func(h *fakeHost, argv []string) {
		if strings.Join(argv, " ") == "nmcli connection up kairoslab0-tap" {
			h.slaves[DefaultBridgeName] = append(h.slaves[DefaultBridgeName], "eth0")
		}
	}
	st := &state.State{}

	err := PrepareLinuxShared(st, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared returned with a host NIC on the bridge it had just built")
	}
	if !strings.Contains(err.Error(), `"eth0"`) {
		t.Errorf("the refusal does not name the port it found:\n%v", err)
	}
	// The tap is a port of this bridge by now and is the one thing expected
	// on it, so it is not named as something that should not be there.
	if strings.Contains(err.Error(), `"`+DefaultTapName+`"`) {
		t.Errorf("the refusal lists the tap among the ports that should not be there:\n%v", err)
	}
	// The whole shared sequence ran, and then the revert.
	want := append(sharedSequence(testUID(t)), refusalTail()...)
	assertSequence(t, h.lines(), want)
	if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
		t.Errorf("state was written for a run that was refused: %+v", st.Network)
	}
}

// What the refusal says about the ports it found, which is where three
// separate wrong answers lived.
func TestPrepareLinuxSharedRefusalNamesEveryPortItFound(t *testing.T) {
	tests := []struct {
		name    string
		ports   []string
		want    []string
		unwant  []string
		rawByte rune
	}{
		{
			// findBridgeSlave returned the FIRST port and the message was
			// singular throughout, so a host with two NICs on the bridge was
			// told about one: the user cleared it, started again, and was
			// refused again naming the second.
			name:  "every port, not the first",
			ports: []string{"eth0", "wlan0"},
			want:  []string{`"eth0"`, `"wlan0"`},
		},
		{
			// `ip` renders a device with a link-layer parent as
			// "<name>@<parent>", so a VLAN port on the bridge reads
			// "eth0.100@eth0" -- and `nmcli device connect eth0.100@eth0`
			// names no device and fails. A VLAN sub-interface on a bridge is
			// an ordinary host layout.
			name:   "a VLAN port is named as a device",
			ports:  []string{"eth0.100@eth0"},
			want:   []string{`"eth0.100"`},
			unwant: []string{"@eth0"},
		},
		{
			// dev_valid_name() bars NUL, '/', ':' and whitespace and nothing
			// else, so an interface really can be named with a raw ESC in it.
			// This message is printed to a terminal by cmd/kairos-lab, the
			// same terminal the cleanup plan is printed to, and the plan
			// neutralises exactly this.
			name:    "a control byte is escaped, not printed",
			ports:   []string{"eth0" + "\x1b" + "[2K"},
			want:    []string{`\x1b`},
			rawByte: 0x1b,
		},
		{
			// And the same for a rune that is printable to the kernel and
			// reorders the line to a reader: U+202E, RIGHT-TO-LEFT OVERRIDE.
			name:    "a direction override is escaped",
			ports:   []string{"eth0" + "\u202e" + "0htew"},
			want:    []string{`\u202e`},
			rawByte: 0x202e,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			ports := tt.ports
			h.onCommand = func(h *fakeHost, argv []string) {
				if strings.Join(argv, " ") == "nmcli connection up kairoslab0" {
					h.slaves[DefaultBridgeName] = ports
				}
			}

			err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false)
			if err == nil {
				t.Fatal("PrepareLinuxShared accepted a bridge with a foreign port on it")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q:\n%v", want, err)
				}
			}
			for _, unwant := range tt.unwant {
				if strings.Contains(err.Error(), unwant) {
					t.Errorf("the refusal still carries %q:\n%v", unwant, err)
				}
			}
			if tt.rawByte != 0 && strings.ContainsRune(err.Error(), tt.rawByte) {
				t.Errorf("the refusal carries %U through to the terminal unescaped:\n%v", tt.rawByte, err)
			}
		})
	}
}

// What the refusal claims about what it saw, and what it offers as a way out.
//
// The message used to assert "Some NetworkManager profile is attaching %s to
// this bridge" and send the user to `nmcli connection show --active`. What
// the code observed is a kernel port; it never looked at a profile, and the
// state its own comment documents -- a bridge left behind by a NetworkManager
// restart, with eth0 still on it and the `ip link delete` skipped -- has no
// live profile to find. The user ran the suggested command, found nothing and
// was offered no next step.
func TestPrepareLinuxSharedRefusalDoesNotInventAProfile(t *testing.T) {
	h := newFakeHost(t)
	h.onCommand = func(h *fakeHost, argv []string) {
		if strings.Join(argv, " ") == "nmcli connection up kairoslab0" {
			h.slaves[DefaultBridgeName] = []string{"eth0"}
		}
	}

	err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared accepted a bridge with a foreign port on it")
	}
	msg := err.Error()

	// It says what it read and where from, and hedges what it did not read.
	for _, want := range []string{
		"ip -o link show master kairoslab0",
		"is not claimed here",
		"likely cause",
		// The remedy that works when there is no profile at all.
		"sudo ip link delete kairoslab0",
		"nomaster",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(msg, "Some NetworkManager profile is attaching") {
		t.Errorf("the refusal asserts a profile it never looked for:\n%v", err)
	}
	// `nmcli device connect <iface>` activates whichever profile
	// NetworkManager rates best for the device, which after a bridged run is
	// the bridge-slave profile -- the advice used to be exactly the command
	// that puts the interface back on a bridge.
	if strings.Contains(msg, "sudo nmcli device connect eth0") {
		t.Errorf("the refusal tells the user to run the command that can re-attach the interface:\n%v", err)
	}
	if !strings.Contains(msg, "sudo nmcli connection up <profile>") {
		t.Errorf("the refusal does not say how to put the interface back safely:\n%v", err)
	}
}

// A refusal may not leave ipv4.method shared persisted on the host.
//
// By the time any check fires, `nmcli connection modify <bridge> ...
// ipv4.method shared` has returned and NetworkManager has written a keyfile.
// A refusal that only deactivated left that profile behind -- alongside the
// profile attaching an interface to the same bridge, which ordinarily carries
// autoconnect yes. The next time that interface came up, NetworkManager
// activated its controller and the NIC landed on a bridge with a DHCP server,
// IPv4 forwarding and a MASQUERADE rule, with kairos-lab not running.
func TestPrepareLinuxSharedRefusalRevertsWhatItWrote(t *testing.T) {
	t.Run("the profiles go", func(t *testing.T) {
		h := newFakeHost(t)
		h.onCommand = func(h *fakeHost, argv []string) {
			if strings.Join(argv, " ") == "nmcli connection up kairoslab0" {
				h.slaves[DefaultBridgeName] = []string{"eth0"}
			}
		}

		err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false)
		if err == nil {
			t.Fatal("PrepareLinuxShared accepted a bridge with a foreign port on it")
		}
		for _, conn := range []string{DefaultBridgeName, DefaultBridgeName + "-tap"} {
			if h.conns[conn] {
				t.Errorf("connection %s is still on the host after a refusal, carrying what this start wrote to it", conn)
			}
		}
		if !strings.Contains(err.Error(), "is off this host again") {
			t.Errorf("the refusal does not say the shared method was taken back off:\n%v", err)
		}
	})

	// And when the revert itself fails, the message says so and names the
	// command that finishes it. Claiming a revert that did not happen is the
	// same error as not reverting.
	t.Run("the delete fails and the message says so", func(t *testing.T) {
		h := newFakeHost(t)
		h.onCommand = func(h *fakeHost, argv []string) {
			if strings.Join(argv, " ") == "nmcli connection up kairoslab0" {
				h.slaves[DefaultBridgeName] = []string{"eth0"}
			}
		}
		h.failCmd = func(argv []string) error {
			if strings.Join(argv, " ") == "nmcli connection delete kairoslab0" {
				return fmt.Errorf("exit status 1")
			}
			return nil
		}

		err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false)
		if err == nil {
			t.Fatal("PrepareLinuxShared accepted a bridge with a foreign port on it")
		}
		if !h.conns[DefaultBridgeName] {
			t.Fatal("the fixture did not leave the bridge connection behind, so this test proves nothing")
		}
		for _, want := range []string{
			"is still on this host",
			"sudo nmcli connection delete kairoslab0",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q:\n%v", want, err)
			}
		}
		if strings.Contains(err.Error(), "is off this host again") {
			t.Errorf("the refusal claims a revert that failed:\n%v", err)
		}
	})
}

// The other half of that decision: bridged carries on over exactly the same
// failure, and must keep doing so.
//
// The orphan is a connection the bridged path creates itself, and it recreates
// and re-modifies it a few commands later -- so a delete that failed leaves it
// with a connection it is about to overwrite with the settings it wants,
// rather than with a NIC enslaved to a NAT bridge. Refusing here would turn a
// survivable leftover into a start the user cannot make without hand-editing
// NetworkManager.
func TestPrepareLinuxBridgeSurvivesAStaleUplinkThatWillNotGo(t *testing.T) {
	h := newFakeHost(t)
	h.links["eth0"] = true
	h.conns[DefaultBridgeName+"-uplink"] = true
	h.failCmd = func(argv []string) error {
		if strings.Join(argv, " ") == "nmcli connection delete kairoslab0-uplink" {
			return fmt.Errorf("exit status 1")
		}
		return nil
	}
	st := &state.State{}
	st.Network.BridgeInterface = "eth0"

	if err := PrepareLinuxBridge(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxBridge refused a leftover it rebuilds itself: %v", err)
	}

	uid := testUID(t)
	// The bridged sequence, minus the `add` for the uplink connection: the
	// delete failed, so the connection is still there and only the modify
	// runs. That modify is the reason this is survivable -- it sets the
	// master, the slave type and the autoconnect this run wants.
	want := []string{
		"nmcli connection delete kairoslab0-uplink",
		"nmcli connection add type bridge ifname kairoslab0 con-name kairoslab0 autoconnect yes stp no",
		"nmcli connection modify kairoslab0 connection.interface-name kairoslab0 ipv4.method auto ipv6.method auto bridge.stp no connection.autoconnect yes",
		"nmcli connection modify kairoslab0-uplink connection.interface-name eth0 master kairoslab0 slave-type bridge connection.autoconnect yes",
		"nmcli connection add type tun ifname kairoslab-tap0 con-name kairoslab0-tap mode tap owner " + uid + " master kairoslab0 slave-type bridge autoconnect yes",
		"nmcli connection modify kairoslab0-tap connection.interface-name kairoslab-tap0 tun.mode tap tun.owner " + uid + " master kairoslab0 slave-type bridge connection.autoconnect yes",
		"nmcli connection up kairoslab0",
		"nmcli connection up kairoslab0-uplink",
		"nmcli connection up kairoslab0-tap",
	}
	assertSequence(t, h.lines(), want)
	if st.Network.Mode != "bridged" {
		t.Errorf("Mode = %q, want %q", st.Network.Mode, "bridged")
	}
}

func TestPrepareLinuxSharedRecordsState(t *testing.T) {
	newFakeHost(t)
	st := &state.State{}
	// An earlier bridged run left an uplink in state; shared has none.
	st.Network.BridgeInterface = "eth0"

	if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxShared: %v", err)
	}

	if st.Network.Mode != "shared" {
		t.Errorf("Mode = %q, want %q", st.Network.Mode, "shared")
	}
	if st.Network.BridgeInterface != "" {
		t.Errorf("BridgeInterface = %q, want it cleared: shared enslaves no uplink", st.Network.BridgeInterface)
	}
	want := sharedLeaseFilePath(DefaultBridgeName)
	if st.Network.DHCPLeaseFile != want {
		t.Errorf("DHCPLeaseFile = %q, want %q", st.Network.DHCPLeaseFile, want)
	}
	if !strings.Contains(st.Network.DHCPLeaseFile, DefaultBridgeName) {
		t.Errorf("DHCPLeaseFile %q does not name the bridge that carries ipv4.method shared", st.Network.DHCPLeaseFile)
	}
	if st.Network.BridgeName != DefaultBridgeName || st.Network.TapName != DefaultTapName {
		t.Errorf("BridgeName/TapName = %q/%q, want %q/%q",
			st.Network.BridgeName, st.Network.TapName, DefaultBridgeName, DefaultTapName)
	}
	if !st.Network.CleanupRequired || !st.Network.CreatedByKairosLab {
		t.Errorf("CleanupRequired=%v CreatedByKairosLab=%v, want both true",
			st.Network.CleanupRequired, st.Network.CreatedByKairosLab)
	}
}

func TestPrepareLinuxBridgeSequence(t *testing.T) {
	h := newFakeHost(t)
	h.links["eth0"] = true
	st := &state.State{}
	st.Network.BridgeInterface = "eth0" // skips detectDefaultUplink, which shells out

	if err := PrepareLinuxBridge(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxBridge: %v", err)
	}

	assertSequence(t, h.lines(), bridgedSequence(testUID(t)))

	if st.Network.Mode != "bridged" {
		t.Errorf("Mode = %q, want %q", st.Network.Mode, "bridged")
	}
	if st.Network.BridgeInterface != "eth0" {
		t.Errorf("BridgeInterface = %q, want %q", st.Network.BridgeInterface, "eth0")
	}
}

// Regression test for a shared-mode lease path surviving into bridged state.
//
// `start --network shared` then `start --network bridged` runs no cleanup in
// between, so PrepareLinuxBridge inherits whatever the shared run wrote. The
// only writer of DHCPLeaseFile is the shared path, so a bridged run that
// merely does not write the field leaves the previous run's value in place,
// and the reader of that field reports a previous guest's address.
func TestPrepareLinuxBridgeClearsDHCPLeaseFile(t *testing.T) {
	h := newFakeHost(t)
	h.links["eth0"] = true
	st := &state.State{}
	st.Network.BridgeInterface = "eth0"
	st.Network.Mode = "shared"
	st.Network.DHCPLeaseFile = sharedLeaseFilePath(DefaultBridgeName)
	if st.Network.DHCPLeaseFile == "" {
		t.Fatal("fixture is empty, the test would pass for the wrong reason")
	}

	if err := PrepareLinuxBridge(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxBridge: %v", err)
	}

	if st.Network.DHCPLeaseFile != "" {
		t.Errorf("DHCPLeaseFile = %q after a bridged run, want it cleared: there is no NetworkManager DHCP server in bridged mode",
			st.Network.DHCPLeaseFile)
	}
}

// Regression test for an unvalidated interface name from state.json reaching
// a root-run command.
//
// The half that matters is that NOTHING is issued. The stale-cleanup block in
// the preflight hands the stored name straight to `nmcli connection delete`
// and `ip link delete`, so a name that gets as far as that block is already a
// deleted host network -- which is why the validator is placed ahead of it
// rather than anywhere else in the preflight. Each case seeds the fake host
// with the malformed name PRESENT as a connection and a link, so a validator
// that were removed, or moved back below the stale block, would leave the
// preflight finding it "stale" and deleting it.
// TestPrepareLinuxSharedRejectsStoredNamesBeforeIssuingAnything no longer
// carries a tap-name case (M2): the tap identity PrepareLinuxShared uses is
// TapNameForIndex(index), computed from a validated integer, and
// st.Network.TapName is never read to build it any more -- it is always
// recomputed from the index and never trusted. A malformed value stored
// there is inert -- there is nothing left for it to reach -- so a test
// asserting it gets refused would be pinning dead code. TestVMFieldsAreNeverTrustedForTapIdentity
// below is the test that replaces it: it plants a malformed TapName in state
// and asserts PrepareLinuxShared runs right past it.
func TestPrepareLinuxSharedRejectsStoredNamesBeforeIssuingAnything(t *testing.T) {
	tests := []struct {
		name   string
		bridge string
		tap    string
	}{
		{"bridge name is a NetworkManager profile", attackNMProfileName, ""},
		{"bridge name walks out of NMSTATEDIR", attackPathTraversal, ""},
		{"bridge name forges a plan row", attackPlanRowInjection, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			st := &state.State{}
			st.Network.BridgeName = tt.bridge
			st.Network.TapName = tt.tap
			for _, name := range []string{tt.bridge, tt.tap} {
				if name == "" {
					continue
				}
				h.conns[name] = true
				h.links[name] = true
				h.bridges[name] = true
			}

			err := PrepareLinuxShared(st, t.TempDir(), 0, false)
			if err == nil {
				t.Fatalf("PrepareLinuxShared accepted a malformed stored name")
			}
			if len(h.commands) != 0 {
				t.Errorf("rejected input still reached root:\n  %s", strings.Join(h.lines(), "\n  "))
			}
			if !strings.Contains(err.Error(), "stored configuration") {
				t.Errorf("error %q does not say where the value came from", err)
			}
			if st.Network.Mode != "" || st.Network.DHCPLeaseFile != "" {
				t.Errorf("state was written despite the error: Mode=%q DHCPLeaseFile=%q",
					st.Network.Mode, st.Network.DHCPLeaseFile)
			}
		})
	}
}

// TestVMFieldsAreNeverTrustedForTapIdentity pins that a stored tap name is
// always recomputed from the index and never trusted: a malformed
// st.Network.TapName -- the kind of value TestPrepareLinuxSharedRejectsStoredNamesBeforeIssuingAnything
// used to refuse -- does not stop a start and is not the name anything ends
// up on the wire. Only TapNameForIndex(index) is ever used.
func TestVMFieldsAreNeverTrustedForTapIdentity(t *testing.T) {
	h := newFakeHost(t)
	st := &state.State{}
	st.Network.TapName = attackNMProfileName

	if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("a malformed stored TapName should not affect a start that no longer reads it: %v", err)
	}
	for _, argv := range h.commands {
		for _, word := range argv {
			if word == attackNMProfileName {
				t.Errorf("the malformed stored name %q reached a root command: %v", attackNMProfileName, argv)
			}
		}
	}
	if st.Network.TapName != DefaultTapName {
		t.Errorf("st.Network.TapName after a successful start = %q, want %q (TapNameForIndex(0), not the stale stored value)", st.Network.TapName, DefaultTapName)
	}
}

// Shared mode's connections must not come up on their own. A bridge carrying
// ipv4.method shared runs a DHCP server and a DNS forwarder and installs a
// MASQUERADE rule, and NetworkManager persists these connections as keyfiles:
// with autoconnect on, one `--network shared` run would put all of that on
// every subsequent boot. kairos-lab activates both explicitly on each start,
// so a start still works; what is given up is re-activation after a
// NetworkManager restart, which leaves a running guest's network down until
// the next start. See prepareLinuxSharedWithNM for the trade in full.
// Bridged is shipped behaviour and keeps autoconnect yes.
func TestSharedDisablesAutoconnectAndBridgedKeepsIt(t *testing.T) {
	t.Run("shared", func(t *testing.T) {
		h := newFakeHost(t)
		if err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false); err != nil {
			t.Fatalf("PrepareLinuxShared: %v", err)
		}
		assertAutoconnect(t, h, "no")
	})
	t.Run("bridged", func(t *testing.T) {
		h := newFakeHost(t)
		h.links["eth0"] = true
		st := &state.State{}
		st.Network.BridgeInterface = "eth0"
		if err := PrepareLinuxBridge(st, t.TempDir(), 0, false); err != nil {
			t.Fatalf("PrepareLinuxBridge: %v", err)
		}
		assertAutoconnect(t, h, "yes")
	})
}

// assertAutoconnect checks that every connection built in the recorded
// sequence sets autoconnect to want, on both the add and the modify.
func assertAutoconnect(t *testing.T, h *fakeHost, want string) {
	t.Helper()
	seen := 0
	for _, argv := range h.commands {
		for _, key := range []string{"autoconnect", "connection.autoconnect"} {
			v := valueAfter(argv, key)
			if v == "" {
				continue
			}
			seen++
			if v != want {
				t.Errorf("%s = %q, want %q in: %s", key, v, want, strings.Join(argv, " "))
			}
		}
	}
	if seen < 4 {
		t.Errorf("found %d autoconnect settings, want at least 4 (an add and a modify, for the bridge and for the tap)", seen)
	}
}

// The preflight and HasStaleNetworkResources must agree about what stale
// means: the preflight seeing less than the reporter is how a resource gets
// left on the host by a run that said it cleaned up.
func TestStalenessPredicateCoversEveryResource(t *testing.T) {
	tests := []struct {
		name string
		seed func(*fakeHost)
	}{
		{"bridge link", func(h *fakeHost) { h.bridges[DefaultBridgeName] = true }},
		{"bridge connection", func(h *fakeHost) { h.conns[DefaultBridgeName] = true }},
		{"uplink connection", func(h *fakeHost) { h.conns[DefaultBridgeName+"-uplink"] = true }},
		{"tap connection", func(h *fakeHost) { h.conns[DefaultBridgeName+"-tap"] = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			tt.seed(h)
			if !hasStaleBridgeResources(DefaultBridgeName, DefaultBridgeName+"-tap") {
				t.Errorf("hasStaleBridgeResources missed a leftover %s", tt.name)
			}
			st := &state.State{}
			if !HasStaleNetworkResources(st) {
				t.Errorf("HasStaleNetworkResources missed a leftover %s", tt.name)
			}
		})
	}

	t.Run("clean host", func(t *testing.T) {
		newFakeHost(t)
		if hasStaleBridgeResources(DefaultBridgeName, DefaultBridgeName+"-tap") {
			t.Error("hasStaleBridgeResources reports a clean host as stale")
		}
	})
}

// tapOwnerUID hands the tap to the user who will run QEMU. SUDO_USER is the
// reason the environment is consulted at all and stays first; everything else
// is os.Getuid(), because a guess is worse than the kernel's own answer -- the
// old chain fell through $USER to a literal "root" and handed the tap to uid 0
// under a systemd unit, cron or `env -i`, where neither variable is set.
func TestTapOwnerUIDFallsBackToTheRealUID(t *testing.T) {
	tests := []struct {
		name      string
		sudoUser  string
		user      string
		wantOwnUI bool
	}{
		{"neither variable set", "", "", true},
		{"only USER set, and it is someone else", "", "nobody", true},
		{"only USER set, and it is root", "", "root", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SUDO_USER", tt.sudoUser)
			t.Setenv("USER", tt.user)
			got, err := tapOwnerUID()
			if err != nil {
				t.Fatalf("tapOwnerUID: %v", err)
			}
			if want := strconv.Itoa(os.Getuid()); got != want {
				t.Errorf("tapOwnerUID() = %q, want %q (the uid this process actually runs as)", got, want)
			}
		})
	}
}

// SUDO_USER still wins when it is set: an unresolvable one is an error, not a
// silent fall through to the current uid, which under sudo would be root.
func TestTapOwnerUIDHonoursSudoUserFirst(t *testing.T) {
	t.Setenv("SUDO_USER", "kairoslab-no-such-user")
	t.Setenv("USER", "")
	if got, err := tapOwnerUID(); err == nil {
		t.Errorf("tapOwnerUID() = %q, nil; want an error for an unresolvable SUDO_USER", got)
	}
}

// reset and cleanup reach cleanupNMConnections without passing through
// linuxNetworkPreflight, so the validator there does not protect them. The
// stored name lands in `sudo nmcli connection delete <name>` and
// `sudo ip link delete <name>`, which for a stored name of "eth0" on a distro
// that names profiles after devices takes out the host's own network. The
// guard has to sit at the choke point as well as at the entry point.
func TestCleanupNMConnectionsRefusesAMalformedStoredName(t *testing.T) {
	for _, bridge := range []string{
		attackNMProfileName,
		attackPathTraversal,
		attackPlanRowInjection,
		"",
	} {
		t.Run(fmt.Sprintf("%q", bridge), func(t *testing.T) {
			h := newFakeHost(t)
			// Make every resource look present, so the only thing standing
			// between the name and root is the validator.
			h.conns[bridge] = true
			h.conns[bridge+"-tap"] = true
			h.conns[bridge+"-uplink"] = true
			h.links[bridge] = true
			h.bridges[bridge] = true

			err := cleanupNMConnections(bridge, DefaultTapName, bridge+"-tap", false)
			if err == nil {
				t.Fatalf("cleanupNMConnections accepted a malformed stored name")
			}
			if len(h.commands) != 0 {
				t.Errorf("rejected input still reached root:\n  %s", strings.Join(h.lines(), "\n  "))
			}
			if !strings.Contains(err.Error(), "refusing to clean up") {
				t.Errorf("error %q does not say it declined to act", err)
			}
		})
	}
}

// The guard must not block the ordinary case it sits in front of.
func TestCleanupNMConnectionsStillRemovesAValidBridge(t *testing.T) {
	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true
	h.conns[DefaultBridgeName+"-tap"] = true
	h.bridges[DefaultBridgeName] = true

	if err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false); err != nil {
		t.Fatalf("cleanupNMConnections(%q) = %v, want nil", DefaultBridgeName, err)
	}
	joined := strings.Join(h.lines(), "\n")
	for _, want := range []string{
		"nmcli connection delete " + DefaultBridgeName + "-tap",
		"nmcli connection delete " + DefaultBridgeName,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

// The reconnect is the last thing a teardown does and the only command in it
// that puts the host back the way it was: deleting the bridge leaves whatever
// was enslaved to it with no active connection, and `nmcli device connect
// <iface>` is what gives that interface one again.
//
// The tap is seeded as a slave in every case here, because it always is one:
// the fake renders real `ip -o link show master` output and findBridgeSlave
// parses it for real, so whether the tap or the host NIC comes back is
// decided by the filter under test and not by the fake.
//
// The reconnect must also not fire when there is nothing to put back. A
// shared-mode bridge has no physical slave -- its only port is the tap -- and
// reconnecting a device that was never enslaved is an unrequested change to
// the host's networking.
func TestCleanupNMConnectionsReconnectsAnEnslavedHostNIC(t *testing.T) {
	t.Run("bridge with a physical slave", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-uplink"] = true
		h.bridges[DefaultBridgeName] = true
		// The tap is listed first, as the kernel would list it, so returning
		// the first line instead of the first non-tap line is visible here.
		h.slaves[DefaultBridgeName] = []string{DefaultTapName, "eth0"}

		if err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false); err != nil {
			t.Fatalf("cleanupNMConnections(%q) = %v, want nil", DefaultBridgeName, err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0-uplink",
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab0",
			// After the deletes, never before: the point of reconnecting is to
			// replace the slave profile that has just been removed.
			"nmcli device connect eth0",
		})
	})

	t.Run("bridge whose only slave is the tap", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-tap"] = true
		h.bridges[DefaultBridgeName] = true
		h.links[DefaultTapName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName}

		if err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false); err != nil {
			t.Fatalf("cleanupNMConnections(%q) = %v, want nil", DefaultBridgeName, err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0-tap",
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab-tap0",
			"ip link delete kairoslab0",
		})
		for _, line := range h.lines() {
			if strings.Contains(line, "device connect") {
				t.Errorf("reconnected a device that was never enslaved: %s", line)
			}
		}
	})

	// The substring filter this replaced skipped any interface whose name
	// merely contained "tap", so a host NIC called "captap0" was left with no
	// active connection after a teardown and nothing said so.
	t.Run("host NIC whose name contains tap", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-uplink"] = true
		h.bridges[DefaultBridgeName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName, "captap0"}

		if err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false); err != nil {
			t.Fatalf("cleanupNMConnections(%q) = %v, want nil", DefaultBridgeName, err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0-uplink",
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab0",
			"nmcli device connect captap0",
		})
	})
}

// The reconnect may not put back what the teardown has just removed.
//
// `nmcli device connect <iface>` activates whichever profile NetworkManager
// rates best for the device, and after a bridged run that is routinely
// <bridge>-uplink -- `master <bridge> slave-type bridge`, autoconnect on.
// Issued while that profile is still on the host it has NetworkManager
// re-create the bridge deleted a line earlier and re-enslave the interface,
// and cleanupNMConnections then joins no errors: `reset` prints "reset
// complete" over a host that has its bridge and its uplink back. This tool
// warns about exactly that command twice, in the preflight refusal and in
// refuseForeignBridgePort, and this is the one place it runs it itself.
//
// Both states below reach the reconnect with that profile in place, and
// neither is exotic.
func TestCleanupNMConnectionsDoesNotReconnectOverASurvivingUplinkProfile(t *testing.T) {
	// The probe is fail-open: nmConnectionExists reports "does not exist"
	// for any nmcli exit it dislikes, a restarting NetworkManager included.
	// Every connection a finished bridged run leaves is really on this host
	// here and no probe can see one of them, so the gated deletes issue
	// nothing at all.
	t.Run("the connection probes are blind to the profiles", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-uplink"] = true
		h.conns[DefaultBridgeName+"-tap"] = true
		h.bridges[DefaultBridgeName] = true
		h.links[DefaultTapName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName, "eth0"}
		nmConnectionExists = func(string) bool { return false }

		var err error
		out := captureStdout(t, func() {
			err = cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false)
		})
		if err != nil {
			t.Fatalf("cleanupNMConnections(%q) = %v, want nil", DefaultBridgeName, err)
		}

		// The two links are the whole of what this teardown could see to
		// remove. The reconnect that used to follow them is what had
		// NetworkManager build the bridge again from the profile that is
		// still there.
		assertSequence(t, h.lines(), []string{
			"ip link delete kairoslab-tap0",
			"ip link delete kairoslab0",
		})
		assertReconnectDeclined(t, out, "eth0")
		// The fixture, not the code, would be what the assertion above was
		// measuring if the delete had gone through.
		if !h.conns[DefaultBridgeName+"-uplink"] {
			t.Fatal("the uplink profile left the host, so this test proves nothing about a surviving one")
		}
	})

	// Nothing is blind here. The profile is visible, the delete of it was
	// issued and it failed, so what is on the host afterwards is the same
	// profile -- and the reconnect is the same mistake.
	t.Run("the uplink delete failed", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-uplink"] = true
		h.bridges[DefaultBridgeName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName, "eth0"}
		h.failCmd = func(argv []string) error {
			if strings.Join(argv, " ") == "nmcli connection delete kairoslab0-uplink" {
				return fmt.Errorf("exit status 1")
			}
			return nil
		}

		var err error
		out := captureStdout(t, func() {
			err = cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false)
		})
		if err == nil {
			t.Fatal("cleanupNMConnections reported success although a delete failed")
		}
		if !strings.Contains(err.Error(), "delete connection kairoslab0-uplink") {
			t.Errorf("the error does not name the delete that failed:\n%v", err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0-uplink",
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab0",
		})
		assertReconnectDeclined(t, out, "eth0")
	})
}

// assertReconnectDeclined reads the notice a teardown prints in place of the
// reconnect. It is the whole of what such a user is told, so it has to name
// the interface left alone, the connection that is the reason, and the way to
// put that interface back that does not pick the profile for them.
func assertReconnectDeclined(t *testing.T, out, iface string) {
	t.Helper()
	for _, want := range []string{
		"Not reconnecting " + strconv.Quote(iface),
		DefaultBridgeName + "-uplink",
		"nmcli device status",
		"sudo nmcli connection up <profile>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the notice does not mention %q:\n%s", want, out)
		}
	}
}

// The notice is printed to a terminal and names an interface that came off
// the kernel through findBridgeSlave and passed no validator, three times.
// TestCleanupNMConnectionsQuotesTheInterfaceItReconnects, further down this
// file, pins the same property for the branch that reconnects; a name is no
// less hostile on the branch that does not.
func TestCleanupNMConnectionsQuotesTheInterfaceItDeclinesToReconnect(t *testing.T) {
	// Assembled from pieces so the literal in this file is not itself a
	// control byte. 12 bytes, so the kernel would take it.
	hostile := "eth0" + "\x1b" + "[2K" + "\x1b" + "[1G"

	h := newFakeHost(t)
	// No connection of ours anywhere, which is the state that declines: the
	// bridge and its port outlived a NetworkManager restart that took the
	// keyfiles with it.
	h.bridges[DefaultBridgeName] = true
	h.slaves[DefaultBridgeName] = []string{DefaultTapName, hostile}

	var err error
	out := captureStdout(t, func() {
		err = cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false)
	})
	if err != nil {
		t.Fatalf("cleanupNMConnections(%q) = %v, want nil", DefaultBridgeName, err)
	}
	if !strings.Contains(out, strconv.Quote(hostile)) {
		t.Errorf("the notice does not quote the interface name: %q", out)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("the notice put a raw escape byte on the terminal: %q", out)
	}
}

// Why the deletes above stay gated on nmConnectionExists, while
// revertSharedSetup deletes the same three names unconditionally and says why
// it does.
//
// The two return values are read differently. revertSharedSetup's is prose
// inside an error already being returned; this one's is a decision -- `reset`
// prints it in place of "reset complete", and linuxNetworkPreflight refuses a
// shared start on it. `nmcli connection delete` fails for a name that is not
// on the host, and <bridge>-uplink is exactly that after every shared run,
// since shared creates none.
//
// So both cases below are ordinary runs that an unconditional delete turns
// into a failure and a refusal on any host with nmcli on it. Neither shows up
// against a fake that fails nothing: the failing command here is the fake
// answering the way nmcli does.
func TestCleanupNMConnectionsIssuesNoDeleteForAConnectionItCannotSee(t *testing.T) {
	// failUnknownConnectionDeletes is nmcli's answer for `connection delete
	// <name>` when no connection of that name is on the host: a non-zero
	// exit, which sudo hands back as a failed command.
	failUnknownConnectionDeletes := func(h *fakeHost) func([]string) error {
		return func(argv []string) error {
			if len(argv) == 4 && argv[0] == "nmcli" && argv[1] == "connection" && argv[2] == "delete" && !h.conns[argv[3]] {
				return fmt.Errorf("exit status 10")
			}
			return nil
		}
	}

	// What a finished shared run leaves, torn down by `reset`.
	t.Run("an ordinary shared reset", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-tap"] = true
		h.bridges[DefaultBridgeName] = true
		h.links[DefaultTapName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName}
		h.failCmd = failUnknownConnectionDeletes(h)

		st := &state.State{}
		st.Network.Mode = "shared"
		st.Network.CreatedByKairosLab = true
		st.Network.BridgeName = DefaultBridgeName
		st.Network.TapName = DefaultTapName

		if err := CleanupLinuxBridge(st, state.VM{}, false); err != nil {
			t.Fatalf("an ordinary shared teardown reported failure: %v", err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0-tap",
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab-tap0",
			"ip link delete kairoslab0",
		})
	})

	// A leftover bridge DEVICE and no connections at all, which is what a
	// NetworkManager restart leaves behind. The preflight tears it down and
	// the start carries on over it; an unconditional delete would end this
	// run in a refusal naming three connections that were never there.
	t.Run("a shared start over a leftover bridge device", func(t *testing.T) {
		h := newFakeHost(t)
		h.bridges[DefaultBridgeName] = true
		h.failCmd = failUnknownConnectionDeletes(h)

		if err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false); err != nil {
			t.Fatalf("PrepareLinuxShared refused a host whose leftover bridge it removed: %v", err)
		}
		want := append([]string{"ip link delete " + DefaultBridgeName}, sharedSequence(testUID(t))...)
		assertSequence(t, h.lines(), want)
	})
}

// A teardown that could not finish must say so. cleanupNMConnections used to
// print each failure with fmt.Printf and return nil, so the only non-nil
// return was the refusal above: `reset` printed "reset complete" over a host
// that still had every one of these resources on it, and the warnings went to
// process stdout where the app layer's own writer could not see them.
func TestCleanupNMConnectionsReportsEveryFailure(t *testing.T) {
	t.Run("every step fails", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-tap"] = true
		h.conns[DefaultBridgeName+"-uplink"] = true
		h.bridges[DefaultBridgeName] = true
		h.links[DefaultTapName] = true
		h.slaves[DefaultBridgeName] = []string{DefaultTapName, "eth0"}
		h.failCmd = func([]string) error { return fmt.Errorf("exit status 1") }

		err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false)
		if err == nil {
			t.Fatal("cleanupNMConnections reported success after every step failed")
		}

		// Collecting is not stopping: one resource that will not go must not
		// strand the ones behind it. The reconnect is not issued here, and
		// not because a failure stopped anything -- the delete of
		// kairoslab0-uplink failed, so that bridge-slave profile is still on
		// the host and `nmcli device connect eth0` would put eth0 back on a
		// bridge.
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0-tap",
			"nmcli connection delete kairoslab0-uplink",
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab-tap0",
			"ip link delete kairoslab0",
		})

		// And every failure is named, since this error is what the app layer
		// prints in place of "reset complete".
		for _, want := range []string{
			"delete connection kairoslab0-tap",
			"delete connection kairoslab0-uplink",
			"delete connection kairoslab0",
			"delete interface kairoslab-tap0",
			"delete interface kairoslab0",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q:\n%v", want, err)
			}
		}
		// A step that was never issued may not appear among the failures.
		if strings.Contains(err.Error(), "reconnect") {
			t.Errorf("error names a reconnect that was never attempted:\n%v", err)
		}
	})

	t.Run("one step fails", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.conns[DefaultBridgeName+"-tap"] = true
		h.bridges[DefaultBridgeName] = true
		h.links[DefaultTapName] = true
		h.failCmd = func(argv []string) error {
			if strings.Join(argv, " ") == "nmcli connection delete kairoslab0-tap" {
				return fmt.Errorf("exit status 1")
			}
			return nil
		}

		err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false)
		if err == nil {
			t.Fatal("a partial teardown reported success")
		}
		if !strings.Contains(err.Error(), "delete connection kairoslab0-tap") {
			t.Errorf("error does not name the step that failed:\n%v", err)
		}
		if strings.Contains(err.Error(), "delete interface kairoslab0") {
			t.Errorf("error names a step that succeeded:\n%v", err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0-tap",
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab-tap0",
			"ip link delete kairoslab0",
		})
	})

	t.Run("nothing fails", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.bridges[DefaultBridgeName] = true

		if err := cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false); err != nil {
			t.Fatalf("a teardown that did everything asked of it returned %v, want nil", err)
		}
	})
}

// The tap name reaches `sudo ip link delete <name>` exactly as the bridge
// name does, so it is checked at the same choke point and for the same
// reason. Before it was threaded through, this path deleted the constant
// DefaultTapName whatever state.json said -- which left a configured tap on
// the host and needed no validation because no stored value was used.
func TestCleanupNMConnectionsRefusesAMalformedStoredTapName(t *testing.T) {
	for _, tap := range []string{attackNMProfileName, attackPathTraversal, attackPlanRowInjection} {
		t.Run(fmt.Sprintf("%q", tap), func(t *testing.T) {
			h := newFakeHost(t)
			h.conns[DefaultBridgeName] = true
			h.links[tap] = true

			err := cleanupNMConnections(DefaultBridgeName, tap, DefaultBridgeName+"-tap", false)
			if err == nil {
				t.Fatal("cleanupNMConnections accepted a malformed stored tap name")
			}
			if len(h.commands) != 0 {
				t.Errorf("rejected input still reached root:\n  %s", strings.Join(h.lines(), "\n  "))
			}
			if !strings.Contains(err.Error(), "tap name") {
				t.Errorf("error %q does not name the field it came from", err)
			}
		})
	}

	// An empty tap name is not malformed: it is a state file written before
	// the tap was ever created, and it means the default.
	t.Run("empty means the default", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.links[DefaultTapName] = true

		if err := cleanupNMConnections(DefaultBridgeName, "", DefaultBridgeName+"-tap", false); err != nil {
			t.Fatalf("cleanupNMConnections with no stored tap name = %v, want nil", err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0",
			"ip link delete kairoslab-tap0",
		})
	})

	// And a configured one is the tap that gets deleted, which is the point of
	// threading it in at all.
	t.Run("a configured tap is the one deleted", func(t *testing.T) {
		h := newFakeHost(t)
		h.conns[DefaultBridgeName] = true
		h.links["kltap0"] = true

		if err := cleanupNMConnections(DefaultBridgeName, "kltap0", DefaultBridgeName+"-tap", false); err != nil {
			t.Fatalf("cleanupNMConnections = %v, want nil", err)
		}
		assertSequence(t, h.lines(), []string{
			"nmcli connection delete kairoslab0",
			"ip link delete kltap0",
		})
	})
}

// The gate that replaced the probe-level fail-open reintroduced it one layer
// down: `if !isLinuxBridge(bridge) { return nil }` in front of all three port
// checks, where isLinuxBridge is
// `os.Stat("/sys/class/net/<name>/bridge"); return err == nil`.
//
// `err == nil` is the whole predicate, so every stat error collapses to the
// one false. Measured against real os.Stat on this host:
//
//	okbr      err == nil is true    -- a bridge
//	eaccesbr  err == nil is false   -- permission denied
//	notdir    err == nil is false   -- not a directory
//	loop      err == nil is false   -- too many levels of symbolic links
//	gone      err == nil is false   -- no such file or directory
//
// Only the last of those means "no such device, therefore no ports", and it
// is the only one that may skip the check.
//
// The rows below are those classes against the stat that replaced it, which
// asks about /sys/class/net/<name> and not about the "bridge" entry under
// it, and the path decides which of them a host really produces. Permission
// denied is the live one: a /sys/class/net this user cannot read answers
// EACCES for every name in it. ENOTDIR and ELOOP are here because the rule
// is "anything that is not ENOENT refuses" and the rule is what is being
// pinned, not a catalogue of today's kernels -- under the device path
// ENOTDIR needs /sys/class/net itself to be a non-directory, and a symlink
// loop is not reachable in sysfs. /sys/class/net/bonding_masters, the
// standing ENOTDIR example, is not one of these at all: it is a regular file
// directly in /sys/class/net, so the OLD path stats bonding_masters/bridge
// and gets ENOTDIR while this one stats bonding_masters itself and gets a
// clean "exists", after which the port check runs and refuses: `ip` has no
// port list for a name that is not a device, and refuseForeignBridgePort
// fails closed on a probe error.
//
// The host below is the sharpest form of it. `ip` works perfectly, eth0 is
// already a port of the bridge, and the stat is the only thing that cannot
// answer -- so with the gate in front of the checks the port list was never
// read at all, ipv4.method shared was applied over the NIC, and the start
// wrote its state.
func TestPrepareLinuxSharedRefusesWhenTheBridgeStatCannotAnswer(t *testing.T) {
	const statted = "/sys/class/net/" + DefaultBridgeName
	tests := []struct {
		name    string
		statErr error
	}{
		{"permission denied", &fs.PathError{Op: "stat", Path: statted, Err: fs.ErrPermission}},
		{"not a directory", &fs.PathError{Op: "stat", Path: statted, Err: syscall.ENOTDIR}},
		{"symlink loop", &fs.PathError{Op: "stat", Path: statted, Err: syscall.ELOOP}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.bridges[DefaultBridgeName] = true
			h.invisibleLinks[DefaultBridgeName] = true
			h.slaves[DefaultBridgeName] = []string{"eth0"}
			h.invisibleBridges[DefaultBridgeName] = tt.statErr
			st := &state.State{}

			err := PrepareLinuxShared(st, t.TempDir(), 0, false)
			if err == nil {
				t.Fatal("PrepareLinuxShared applied ipv4.method shared over a bridge it never read the port list of")
			}

			// Nothing is activated: the refusal comes from the check that
			// sits before the first `connection up`, so the method is never
			// applied to a bridge carrying a NIC.
			for _, line := range h.lines() {
				if strings.HasPrefix(line, "nmcli connection up") {
					t.Errorf("a connection was activated over a bridge whose ports could not be established: %s", line)
				}
			}
			uid := testUID(t)
			want := append([]string{}, sharedSequence(uid)[:4]...)
			want = append(want, refusalTail()...)
			assertSequence(t, h.lines(), want)

			// And the message is about the stat, not about a port: no port
			// list was read here and the message may not claim one.
			for _, wantText := range []string{
				"/sys/class/net/" + DefaultBridgeName,
				"no such file or directory",
			} {
				if !strings.Contains(err.Error(), wantText) {
					t.Errorf("the refusal does not mention %q:\n%v", wantText, err)
				}
			}
			if strings.Contains(err.Error(), "reports") {
				t.Errorf("the refusal claims a port list it never read:\n%v", err)
			}
			if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
				t.Errorf("state was written for a run that was refused: %+v", st.Network)
			}
		})
	}
}

// The path the device stat is given is the security decision, so it is
// pinned here rather than left to the seam every other test replaces.
//
// It used to be built inside statNetDevice. Changing the join there to
// filepath.Join("/sys/class/net", name, "bridge") -- the predicate the
// device stat was introduced to replace, and one that answers "not there"
// for a bond, a team, a VRF or an OVS bridge that IS there with the host's
// NIC on it -- left the whole suite green, because no test runs the body of
// that var. The join lives in netDevicePath now, and these two assertions
// are what fail when it moves: the first reads the path the production
// caller actually hands the seam, the second spells the literal out so the
// expectation cannot drift with the code it describes.
func TestNetDeviceExistsStatsTheDeviceEntry(t *testing.T) {
	orig := statNetDevice
	t.Cleanup(func() { statNetDevice = orig })

	var statted []string
	statNetDevice = func(path string) error {
		statted = append(statted, path)
		return nil
	}

	exists, err := netDeviceExists(DefaultBridgeName)
	if err != nil {
		t.Fatalf("netDeviceExists = %v, want nil", err)
	}
	if !exists {
		t.Fatal("netDeviceExists answered false for a stat that succeeded")
	}
	want := []string{"/sys/class/net/" + DefaultBridgeName}
	if !slices.Equal(statted, want) {
		t.Errorf("the stat was asked about %q, want %q -- anything else is a different question than \"is a device of this name on the host\"", statted, want)
	}

	// A bond master is the case the device entry exists for: it is in
	// /sys/class/net, `ip -o link show master` lists its ports, and it has no
	// "bridge" entry under it at all.
	if got := netDevicePath("bond0"); got != "/sys/class/net/bond0" {
		t.Errorf("netDevicePath(%q) = %q, want %q", "bond0", got, "/sys/class/net/bond0")
	}
}

// The other side of the same rule, and the reason it is a rule and not a
// blanket refusal: "no such file or directory" IS an answer. A device that is
// not on the host has no ports, and a clean first run has no bridge yet --
// both connections are added with autoconnect no, so nothing of that name
// exists until the explicit `connection up`.
//
// A fix that refused on every stat error without excepting this one would
// refuse every first shared start there is: the port check it would then run
// fails closed on a probe error, and there is no port list on a device that
// is not there.
func TestPrepareLinuxSharedProceedsWhenTheBridgeDeviceIsNotThere(t *testing.T) {
	h := newFakeHost(t)
	h.invisibleBridges[DefaultBridgeName] = &fs.PathError{
		Op:   "stat",
		Path: "/sys/class/net/" + DefaultBridgeName,
		Err:  fs.ErrNotExist,
	}
	st := &state.State{}

	if err := PrepareLinuxShared(st, t.TempDir(), 0, false); err != nil {
		t.Fatalf("PrepareLinuxShared refused a host that simply has no bridge of that name yet: %v", err)
	}
	assertSequence(t, h.lines(), sharedSequence(testUID(t)))
	if st.Network.Mode != "shared" {
		t.Errorf("Mode = %q, want %q", st.Network.Mode, "shared")
	}
}

// The second class the gate let through, and the one no stat error is needed
// for: a master that exists, has ports, and is not a Linux bridge.
//
// `ip -o link show master <dev>` lists the ports of ANY master -- a bond, a
// team, a VRF, an OVS bridge. /sys/class/net/<dev>/bridge exists only for a
// Linux bridge, so isLinuxBridge answers false for every one of them while
// the device sits there with the host's NIC on it. On the host this was
// measured on, isLinuxBridge("eth0") is false and eth0 exists.
//
// The name comes out of state.json, which is a 0644 file, and
// validateStoredInterfaceName accepts "bond0" in it.
func TestPrepareLinuxSharedRefusesPortsOnAMasterThatIsNotABridge(t *testing.T) {
	h := newFakeHost(t)
	// A device of the bridge's name that is not a bridge: no entry in
	// h.bridges, so isLinuxBridge answers false, exactly as /sys does for a
	// bond master.
	h.links[DefaultBridgeName] = true
	h.slaves[DefaultBridgeName] = []string{"eth0"}
	st := &state.State{}

	err := PrepareLinuxShared(st, t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("PrepareLinuxShared brought a NAT bridge up over a master carrying the host's NIC")
	}
	if !strings.Contains(err.Error(), `"eth0"`) {
		t.Errorf("the refusal does not name the port it found:\n%v", err)
	}

	// Before any activation, which is the whole value of the check that runs
	// there: ipv4.method shared is written to the profile by then, and
	// bringing it up is what applies it to the device.
	for _, line := range h.lines() {
		if strings.HasPrefix(line, "nmcli connection up") {
			t.Errorf("a connection was activated over a master carrying a host NIC: %s", line)
		}
	}
	uid := testUID(t)
	want := append([]string{}, sharedSequence(uid)[:4]...)
	want = append(want, refusalTail()...)
	assertSequence(t, h.lines(), want)
	if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
		t.Errorf("state was written for a run that was refused: %+v", st.Network)
	}
}

// revertSharedSetup's own rule -- "a refusal has to leave the host no more
// dangerous than it found it" -- applied to the exits that are not refusals.
//
// `nmcli connection modify <bridge> ... ipv4.method shared` persists that
// setting to a keyfile the moment it returns, and four steps run after it.
// Each one used to return its error bare, leaving the profile on the host:
// the next time NetworkManager activates it, the device it names runs a DHCP
// server, IPv4 forwarding and a MASQUERADE rule with no VM anywhere near it.
//
// The sharpest of the four is the activation timeout, where nmcli exits
// non-zero AFTER NetworkManager has already activated the bridge and a
// foreign profile has put eth0 on it. That case is seeded below as a command
// that changes the host and then fails, because that is what it does.
func TestPrepareLinuxSharedRevertsEveryExitAfterTheMethodIsWritten(t *testing.T) {
	uid := testUID(t)
	shared := sharedSequence(uid)
	tests := []struct {
		name string
		// failing is the joined argv of the command that fails. The expected
		// sequence is everything up to and including it, then the revert.
		failing string
		// half, when set, is what the failing command did to the host before
		// it failed.
		half func(h *fakeHost)
	}{
		{
			name:    "the tap connection cannot be added",
			failing: shared[2],
		},
		{
			name:    "the tap connection cannot be modified",
			failing: shared[3],
		},
		{
			name:    "the bridge activation times out after NetworkManager activated it",
			failing: shared[4],
			half: func(h *fakeHost) {
				h.activate(DefaultBridgeName)
				h.slaves[DefaultBridgeName] = []string{"eth0"}
			},
		},
		{
			name:    "the tap activation fails",
			failing: shared[5],
			half: func(h *fakeHost) {
				h.activate(DefaultBridgeName)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.failCmd = func(argv []string) error {
				if strings.Join(argv, " ") != tt.failing {
					return nil
				}
				if tt.half != nil {
					tt.half(h)
				}
				return fmt.Errorf("Error: Connection activation failed: Activation failed: timeout")
			}
			st := &state.State{}

			err := PrepareLinuxShared(st, t.TempDir(), 0, false)
			if err == nil {
				t.Fatalf("PrepareLinuxShared reported success although %q failed", tt.failing)
			}

			// The revert ran, and the connection carrying ipv4.method shared
			// is off the host rather than waiting for the next activation.
			var want []string
			for _, line := range shared {
				want = append(want, line)
				if line == tt.failing {
					break
				}
			}
			want = append(want, refusalTail()...)
			assertSequence(t, h.lines(), want)
			if h.conns[DefaultBridgeName] {
				t.Error("the connection carrying ipv4.method shared is still on the host after a failed start")
			}
			if ports := h.slaves[DefaultBridgeName]; len(ports) != 0 {
				t.Errorf("%v left on the bridge after a failed start", ports)
			}

			// And the error says what was done about it, so a user reading
			// it knows whether anything is still there.
			for _, wantText := range []string{
				"ipv4.method shared",
				"is off this host again",
			} {
				if !strings.Contains(err.Error(), wantText) {
					t.Errorf("the error does not mention %q:\n%v", wantText, err)
				}
			}
			if st.Network.Mode != "" || st.Network.CreatedByKairosLab {
				t.Errorf("state was written for a run that failed: %+v", st.Network)
			}
		})
	}
}

// The sibling print of the data quoteNames was written to sanitize.
//
// uplinkIface comes from findBridgeSlave, which reads raw `ip -o link show
// master` output -- the same source, with the same argument: these names
// passed no validator, and dev_valid_name() bars only an empty name, 15 or
// more bytes, "." and "..", and any '/', ':' or whitespace. An ESC is none of
// those. Printed raw, "\x1b[2K\x1b[1G" erases the line the teardown just
// wrote and returns the cursor to column 1.
//
// THREE copies of that name reach a terminal, not two: the progress line,
// the `reconnect %q` wrap, and the argv inside the error that wrap wraps,
// which is the one a reader of that line does not see coming. The third is
// only measurable here because the fake host wraps its failures with
// sudoError, exactly as the real sudo does. While it returned failCmd's
// error bare, this test passed over production code that put a raw ESC on
// the terminal: the assertion read
//
//	reconnect "eth0\x1b[2K\x1b[1G": exit status 1
//
// where the message a user got was
//
//	reconnect "eth0\x1b[2K\x1b[1G": sudo command failed: sudo nmcli device
//	connect <ESC>[2K<ESC>[1G: exit status 1
//
// A fake that differs from production in the very byte under test certifies
// the fake. It does not differ now.
func TestCleanupNMConnectionsQuotesTheInterfaceItReconnects(t *testing.T) {
	// Assembled from pieces so the literal in this file is not itself a
	// control byte. 12 bytes, so the kernel would take it.
	hostile := "eth0" + "\x1b" + "[2K" + "\x1b" + "[1G"

	h := newFakeHost(t)
	h.conns[DefaultBridgeName] = true
	// The reconnect runs for an interface whose uplink connection this
	// teardown deleted, so that connection is part of the fixture.
	h.conns[DefaultBridgeName+"-uplink"] = true
	h.bridges[DefaultBridgeName] = true
	h.links[DefaultTapName] = true
	h.slaves[DefaultBridgeName] = []string{DefaultTapName, hostile}
	h.failCmd = func(argv []string) error {
		if len(argv) > 1 && argv[0] == "nmcli" && argv[1] == "device" {
			return fmt.Errorf("exit status 1")
		}
		return nil
	}

	var err error
	out := captureStdout(t, func() {
		err = cleanupNMConnections(DefaultBridgeName, DefaultTapName, DefaultBridgeName+"-tap", false)
	})
	if err == nil {
		t.Fatal("cleanupNMConnections reported success although the reconnect failed")
	}

	// Both places the name reaches the terminal: the progress line and the
	// error the app layer prints in place of "reset complete".
	for _, where := range []struct{ what, text string }{
		{"the progress line", out},
		{"the error", err.Error()},
	} {
		if !strings.Contains(where.text, strconv.Quote(hostile)) {
			t.Errorf("%s does not quote the interface name: %q", where.what, where.text)
		}
		if strings.ContainsRune(where.text, '\x1b') {
			t.Errorf("%s put a raw escape byte on the terminal: %q", where.what, where.text)
		}
	}

	// And specifically the copy inside the wrapped error: the argv sudoError
	// renders, which no %q at the call site above it can reach.
	wantCmd := "sudo nmcli device connect " + strconv.Quote(hostile)
	if !strings.Contains(err.Error(), wantCmd) {
		t.Errorf("the error does not name the command that failed with its argument escaped:\nwant %s\n got %q", wantCmd, err.Error())
	}
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what
// was written to it. cleanupNMConnections prints its progress line with
// fmt.Printf, which resolves os.Stdout at the call, so this is the only way
// to see it. The output is one short line and fits the pipe buffer, so
// nothing has to drain it while fn runs.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("closing the pipe's write end: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the captured output: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("closing the pipe's read end: %v", err)
	}
	return string(out)
}

// internal/app's consent paragraph says out loud how many times the port list
// is read, so the number is pinned here rather than described. It said
// "three times" while the gate in front of the checks was making it zero on
// whole classes of host.
//
// Two on a clean host and three over a device that is already there, and the
// difference is the pre-activation check: a clean first run has no device of
// the bridge's name, so there is nothing to read a port list off.
func TestPrepareLinuxSharedPortListReadCount(t *testing.T) {
	t.Run("a clean host reads it twice", func(t *testing.T) {
		h := newFakeHost(t)

		if err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false); err != nil {
			t.Fatalf("PrepareLinuxShared = %v, want nil", err)
		}
		if h.slaveLinksCalls != 2 {
			t.Errorf("the port list was read %d times, want 2 (after the bridge is up, and after the tap is on it)", h.slaveLinksCalls)
		}
	})

	t.Run("a device already there is read three times", func(t *testing.T) {
		h := newFakeHost(t)
		// A device of that name with no ports and no connection of ours
		// beside it, so hasStaleBridgeResources finds nothing and no teardown
		// runs -- a teardown reads the same probe once more for its reconnect
		// hint, and that read is not one of the checks this counts.
		h.links[DefaultBridgeName] = true

		if err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false); err != nil {
			t.Fatalf("PrepareLinuxShared = %v, want nil", err)
		}
		if h.slaveLinksCalls != 3 {
			t.Errorf("the port list was read %d times, want 3 (once before anything is activated, and once after each `connection up`)", h.slaveLinksCalls)
		}
	})

	// And the count the escape produced: a stat that cannot answer used to
	// take every one of them away.
	t.Run("an unreadable stat no longer skips them", func(t *testing.T) {
		h := newFakeHost(t)
		h.bridges[DefaultBridgeName] = true
		h.invisibleLinks[DefaultBridgeName] = true
		h.invisibleBridges[DefaultBridgeName] = &fs.PathError{
			Op:   "stat",
			Path: "/sys/class/net/" + DefaultBridgeName,
			Err:  fs.ErrPermission,
		}

		if err := PrepareLinuxShared(&state.State{}, t.TempDir(), 0, false); err == nil {
			t.Fatal("PrepareLinuxShared completed over a host it could not stat")
		}
		// Zero reads here is right and is not the escape: the refusal is the
		// stat's, and it fires before any port list is worth asking for. The
		// escape was zero reads followed by a COMPLETED start, which is what
		// the assertion above forbids.
		if h.slaveLinksCalls != 0 {
			t.Errorf("the port list was read %d times after the stat refused, want 0", h.slaveLinksCalls)
		}
	})
}
