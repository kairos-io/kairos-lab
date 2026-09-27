package vm

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The path building and the output parsing in this file are deliberately kept
// apart from the exec calls in network_linux.go so they can be exercised on
// any host, not only on Linux. network_linux.go carries no build tag of its own: it compiles only on
// Linux by virtue of its _linux.go filename, and a test placed beside it would
// inherit that constraint.

// nmStateDir is NetworkManager's NMSTATEDIR, the directory it keeps the
// dnsmasq lease files of shared connections in.
const nmStateDir = "/var/lib/NetworkManager"

// sharedLeaseFilePath returns the dnsmasq lease file NetworkManager writes for
// an interface running ipv4.method shared, or "" when iface is empty.
//
// NetworkManager starts the shared-mode dnsmasq with
// --dhcp-leasefile=<NMSTATEDIR>/dnsmasq-<iface>.leases, where NMSTATEDIR is
// /var/lib/NetworkManager and <iface> is nm_device_get_ip_iface() of the
// device the method was applied to.
//
// So iface is the interface that ACTUALLY RECEIVED ipv4.method shared, which
// is not the same thing as "the bridge name" even though today they are the
// same string. Move the method onto the tap and the lease file moves with it:
// a caller that recomputed this path from state.Network.BridgeName would then
// read a file nothing writes, or a stale one left by an earlier run, and no
// test here would notice. That is why PrepareLinuxShared calls this once, at
// the point it applies the method, and records the answer in
// state.Network.DHCPLeaseFile for later readers instead of letting them
// derive it again.
func sharedLeaseFilePath(iface string) string {
	if iface == "" {
		return ""
	}
	return filepath.Join(nmStateDir, "dnsmasq-"+iface+".leases")
}

// maxInterfaceNameLen is the longest interface name the Linux kernel accepts.
// IFNAMSIZ is 16 bytes and the last one is the terminating NUL, so 15
// characters is the real limit.
const maxInterfaceNameLen = 15

// validateStoredInterfaceName checks an interface name that came out of
// state.json, before anything is done with it. field names the setting it
// came from so the error points at the thing a user would have to edit.
//
// The names in state.json are not trustworthy input. The file lives in the
// user's config directory with mode 0644, so anything running as the user can
// put a string in it, and the Linux network paths then hand that string to
// `sudo nmcli connection delete <name>` and `sudo ip link delete <name>`
// during stale cleanup. "Wired connection 1" is a real NetworkManager profile
// on most desktops, and on distros that name profiles after the device an
// "eth0" there names the host's own NIC: either one deletes the host's
// network configuration, and the consent prompt that runs alongside names the
// uplink rather than the resource about to be deleted.
//
// The same string reaches two more places. sharedLeaseFilePath joins it into
// a path, where "../../../etc/shadow" escapes NMSTATEDIR -- filepath.Join
// cleans the result, but cleaning is what performs the escape, because the
// literal "dnsmasq-.." component is popped by the ".." that follows it. And
// it is printed into the cleanup plan, where an embedded newline plus a CSI
// sequence forges a plan row and erases the real one after it, subverting the
// confirmation prompt that immediately follows.
//
// This rule closes the first two of those, and only those: no space to
// separate an argument, no dot to walk a path. Both of those consumers sit
// downstream of a call to this function. The cleanup plan does not. It is
// printed before anything validates -- `reset` and `cleanup` build and print
// their plan straight from store.Load(), and only reach the check inside
// cleanupNMConnections after the user has already answered the prompt.
//
// So the third vector is closed at the print boundary instead, by a different
// mechanism than this one. internal/app runs every plan row through its
// planValue helper, which returns a value unchanged when all of its runes are
// printable and hands it to strconv.Quote when any is not -- so a row carrying
// a newline, a CSI sequence or an invalid UTF-8 byte arrives as one escaped
// line, whatever this validator would have made of it. That covers the rows
// these two names appear in and every sibling row beside them: stored paths,
// disk names and dependency names, none of which pass through here at all. The
// errors below quote with %q for the same reason, since they are printed to
// the same terminal and one of them carries the rejected value back to it.
// Two mechanisms and not one; dropping either reopens its own half.
//
// The rule is also deliberately stricter than the kernel, which is what makes
// it usable as an argv and path guard. dev_valid_name() rejects only an empty
// name, a name of IFNAMSIZ bytes or more, exactly "." or "..", and any '/',
// ':' or whitespace -- it accepts a leading '-', and it accepts a dot
// anywhere else. Neither is safe here: a leading '-' is read as an option by
// the nmcli and ip invocations, and a dot is what walks out of NMSTATEDIR.
// The known cost of the narrower rule is that a legitimate VLAN name like
// "eth0.100" is rejected. That is accepted rather than worked around: these
// two fields name the bridge and the tap kairos-lab creates for itself, both
// default to names this rule allows, and nothing in the tool puts a VLAN
// interface in either.
func validateStoredInterfaceName(field, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("invalid %s in stored configuration: the name is empty", field)
	case name == "." || name == "..":
		return fmt.Errorf("invalid %s %q in stored configuration: that is a directory reference, not an interface name", field, name)
	case len(name) > maxInterfaceNameLen:
		return fmt.Errorf("invalid %s %q in stored configuration: %d bytes, but an interface name is at most %d", field, name, len(name), maxInterfaceNameLen)
	case strings.HasPrefix(name, "-"):
		// Stricter than the kernel, which would accept this: a name starting
		// with a dash is read as an option by the nmcli and ip invocations it
		// is passed to, and no real interface is named that way.
		return fmt.Errorf("invalid %s %q in stored configuration: an interface name may not begin with '-'", field, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			continue
		}
		// %q and not %s: the value may carry newlines or escape sequences,
		// and an error message is printed to the same terminal the plan is.
		return fmt.Errorf("invalid %s %q in stored configuration: an interface name may contain only letters, digits, '_' and '-'", field, name)
	}
	return nil
}

// parseBridgeSlave returns the first interface in `ip -o link show master
// <bridge>` output that is not the tap, or "" when the output names none.
// network_linux.go owns the exec that produces out; this half is the part
// worth testing, and it needs no Linux and no bridge to test.
//
// The filter is the whole point of the function. What it returns is the
// interface the teardown hands to `nmcli device connect` -- the one command in
// a teardown that puts the host's own NIC back after the bridge it was
// enslaved to is deleted. Return the tap and the teardown reconnects a device
// nobody enslaved and leaves the real NIC with no active connection; return ""
// when there was a physical slave and the host is left off the network with
// nothing said.
//
// tap is matched by name. The old filter skipped any interface whose name
// merely CONTAINED "tap", so a host NIC called "captap0" or "tap-lan" was
// silently never reconnected. DefaultTapName is skipped alongside the
// configured name so that a tap name edited in state.json after the tap was
// created cannot make the old tap look like a physical slave; for the default
// configuration the two are the same string and the behaviour is unchanged.
//
// This is a teardown's question and not the shared path's assertion, which is
// why parseBridgePorts below exists beside it rather than being built out of
// it: excluding a name is right here and wrong there. See the comment on that
// function.
func parseBridgeSlave(out, tap string) string {
	for _, line := range strings.Split(out, "\n") {
		iface := bridgePortName(line)
		if iface == "" || iface == tap || iface == DefaultTapName {
			continue
		}
		return iface
	}
	return ""
}

// parseBridgePorts returns every interface named in `ip -o link show master
// <bridge>` output, in the order the kernel printed them, with no name
// treated as special.
//
// Nothing is excluded, and that is the difference from parseBridgeSlave. The
// one name a shared-mode port check would have to exclude to reuse that
// function is st.Network.TapName, and that string comes out of state.json --
// a 0644 file anything running as the user can write, and the file the check
// exists to defend against. validateStoredInterfaceName accepts "eth0" there,
// so a check that honoured the exclusion would look straight past the host's
// own NIC sitting on the NAT bridge. The callers say what they expect the
// port list to hold instead, and before the tap is activated that is nothing
// at all.
func parseBridgePorts(out string) []string {
	var ports []string
	for _, line := range strings.Split(out, "\n") {
		if name := bridgePortName(line); name != "" {
			ports = append(ports, name)
		}
	}
	return ports
}

// bridgePortName returns the interface name carried by one line of `ip -o
// link show` output, or "" when the line carries none.
//
// The name is the second field with the ':' the kernel prints after it
// removed, and with anything from an '@' onwards removed too. `ip` renders a
// device that has a link-layer parent as "<name>@<parent>", so a VLAN port
// reads "eth0.100@eth0" and a veth reads "veth7a1b@if12". Only the part
// before the '@' names a device: `nmcli device connect eth0.100@eth0` and
// `ip link set dev veth7a1b@if12 nomaster` both fail, so a message built from
// the untrimmed field sends the user to a command that cannot work, and a
// teardown built from it reconnects nothing. A VLAN sub-interface on a bridge
// is an ordinary host layout rather than a corner case.
func bridgePortName(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	// Format: "3: enp0s31f6: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ..."
	name := strings.TrimSuffix(fields[1], ":")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return name
}

// unexpectedBridgePorts returns the ports that are not in expected, in the
// order they were reported. An empty expected makes every port unexpected,
// which is what the shared path asserts before it activates its tap.
func unexpectedBridgePorts(ports, expected []string) []string {
	var unexpected []string
	for _, port := range ports {
		if slices.Contains(expected, port) {
			continue
		}
		unexpected = append(unexpected, port)
	}
	return unexpected
}

// quoteNames renders interface names for a message: each one quoted, comma
// separated.
//
// These names come from `ip` output and passed no validator on the way --
// unlike the bridge and tap names, which validateStoredInterfaceName has
// narrowed to letters, digits, '_' and '-'. The kernel's own rule is far
// wider: dev_valid_name() bars only an empty name, a name of IFNAMSIZ bytes
// or more, "." and "..", and any '/', ':' or whitespace, so an interface can
// really be named with a raw ESC in it or with U+202E. The errors these go
// into are printed to a terminal by cmd/kairos-lab, which is the same
// boundary internal/app's planValue draws for the cleanup plan; strconv.Quote
// is what that helper uses on a value with an unprintable rune in it, and it
// is what is applied here to every one of them.
func quoteNames(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	return strings.Join(quoted, ", ")
}

// renderArgv renders the command line an exec failed on, for the error that
// reports the failure. Each word is quoted only when it needs to be, and the
// words are joined with spaces, so an ordinary failure still reads
// `sudo nmcli connection delete kairoslab0`.
//
// The words are not all the tool's own. `nmcli device connect <iface>` is
// built from the interface findBridgeSlave read out of `ip -o link show
// master` output, which passed no validator on the way -- dev_valid_name()
// bars only an empty name, IFNAMSIZ bytes or more, "." and "..", and any
// '/', ':' or whitespace, so a raw ESC in an interface name is a name the
// kernel takes. The error built here is wrapped by cleanupNMConnections and
// travels through errors.Join, the shared refusal and internal/app to
// cmd/kairos-lab's fmt.Fprintln(os.Stderr, ...), so it reaches a terminal
// with nothing else looking at it. The caller that quotes its OWN copy of
// that name with %q does not cover this one: both copies are in the same
// string, and the one inside the %w arrived raw.
//
// Quoting every word unconditionally would cover it too and is not what this
// does, for the reason internal/app's planValue gives: a command line that a
// user may have to read, retype or compare against their shell history has
// to stay a command line. A value whose runes are all printable is returned
// unchanged; anything else goes to strconv.Quote, which escapes every rune
// unicode.IsPrint rejects -- C0, DEL, the C1 block including 8-bit CSI, the
// bidi overrides, the zero-width formatters -- and renders bytes that are
// not valid UTF-8 at all as \x escapes.
func renderArgv(argv []string) string {
	words := make([]string, 0, len(argv))
	for _, word := range argv {
		words = append(words, renderArgvWord(word))
	}
	return strings.Join(words, " ")
}

// renderArgvWord renders one word of a command line. The space rule is this
// line's own and sits on top of the printability one, because a word with a
// space in it has to still look like one word: unquoted, `sudo nmcli
// connection delete Wired connection 1` names a command nobody ran. An empty
// word is quoted for the same reason -- it would otherwise vanish into the
// join and leave an argv shorter than the one that failed.
func renderArgvWord(word string) string {
	if word == "" || strings.ContainsAny(word, " \"") || !isPrintableValue(word) {
		return strconv.Quote(word)
	}
	return word
}

// isPrintableValue reports whether s can be written to a terminal as it
// stands. It is the predicate behind internal/app's planValue, spelled out
// here because internal/app imports this package and not the other way
// round, and a value on its way out of internal/vm reaches the same terminal
// through the same cmd/kairos-lab print.
func isPrintableValue(s string) bool {
	// A range loop decodes an invalid byte as utf8.RuneError, and U+FFFD is
	// printable -- so a raw 0x9b (8-bit CSI, invalid on its own in UTF-8)
	// would pass the loop untouched. Reject invalid encoding first.
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		// The three whitespace controls are spelled out although
		// unicode.IsPrint already rejects all three: they are the runes that
		// do the damage, and a reader should not have to know the Cc table
		// to see that they are caught here.
		if !unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' {
			return false
		}
	}
	return true
}

// firstLine returns the first line of s, trimmed of surrounding space and cut
// to at most maxLen bytes, for quoting another program's stderr into an error
// message.
//
// A failing `ip` is worth quoting -- "ip: either \"dev\" is duplicate, or
// \"br0\" is garbage" is what a busybox `ip` says to `show master`, and it
// tells the user exactly which of the causes the message lists they have --
// but it is output from another program and gets neither the terminal nor the
// whole message to itself. The cut is on bytes and may split a rune; the
// caller quotes the result, and strconv.Quote renders an invalid byte as an
// escape rather than passing it through.
func firstLine(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.TrimSpace(s)
}
