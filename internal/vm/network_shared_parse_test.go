package vm

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSharedLeaseFilePath(t *testing.T) {
	tests := []struct {
		name  string
		iface string
		want  string
	}{
		{
			name:  "bridge carrying the shared method",
			iface: "kairoslab0",
			want:  "/var/lib/NetworkManager/dnsmasq-kairoslab0.leases",
		},
		{
			// An empty interface means nothing received the method yet, so
			// there is no lease file to name. Returning the directory with a
			// bare "dnsmasq-.leases" in it would give a reader a path that
			// looks real and never exists.
			name:  "empty iface",
			iface: "",
			want:  "",
		},
		{
			// Hyphens are ordinary characters in the template; the default tap
			// name has one, and it is the interface the method would land on
			// if the shared method ever moved off the bridge.
			name:  "hyphenated interface name",
			iface: "kairoslab-tap0",
			want:  "/var/lib/NetworkManager/dnsmasq-kairoslab-tap0.leases",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sharedLeaseFilePath(tt.iface); got != tt.want {
				t.Errorf("sharedLeaseFilePath(%q) = %q, want %q", tt.iface, got, tt.want)
			}
		})
	}
}

// The lease file is per interface, so the path is only right for the interface
// that actually received ipv4.method shared. A resolver handed the wrong one
// reads the wrong file rather than failing, which is why PrepareLinuxShared
// stores the path it computed instead of letting readers rebuild it.
func TestSharedLeaseFilePathIsPerInterface(t *testing.T) {
	bridge := sharedLeaseFilePath(DefaultBridgeName)
	tap := sharedLeaseFilePath(DefaultTapName)
	if bridge == tap {
		t.Errorf("lease path for %q and %q are both %q, want distinct files",
			DefaultBridgeName, DefaultTapName, bridge)
	}
}

// The three strings below are the ones that were demonstrated to reach a
// root-run command, a path join outside NMSTATEDIR, and the terminal a
// consent prompt is printed to. They are spelled out here rather than
// described so a future relaxation of the rule has to delete a named attack.
const (
	// A real NetworkManager profile name on most desktops. As a stored bridge
	// name it becomes `sudo nmcli connection delete Wired connection 1` and
	// `sudo ip link delete Wired connection 1`.
	attackNMProfileName = "Wired connection 1"
	// sharedLeaseFilePath would join this to /var/lib/NetworkManager and
	// clean it down to /var/lib/etc/shadow.leases, outside NMSTATEDIR.
	attackPathTraversal = "../../../etc/shadow"
	// A forged cleanup-plan row followed by CSI 2K CR, which erases the real
	// row printed after it. Written with Go escapes: no raw control byte ever
	// appears in this source file.
	attackPlanRowInjection = "kairoslab0\n  - eth0 (will be KEPT)\x1b[2K\r"
)

func TestValidateStoredInterfaceNameAccepts(t *testing.T) {
	names := []string{
		DefaultBridgeName,
		DefaultTapName,
		"eth0",
		"enp0s31f6",
		"br-0_1",
		"a",
		"012345678901234", // exactly IFNAMSIZ-1 bytes
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if err := validateStoredInterfaceName("bridge name", name); err != nil {
				t.Errorf("validateStoredInterfaceName(%q) = %v, want nil", name, err)
			}
		})
	}
}

func TestValidateStoredInterfaceNameRejects(t *testing.T) {
	tests := []struct {
		name  string
		value string
		// wantMsg is a fragment only one arm of the validator can produce.
		// "." and ".." are the cases that need it: every other rejection here
		// is the only thing standing between the value and an error, but a
		// dot is already outside the character class, so the arm that names
		// it would be deletable with the whole suite still green if these two
		// asserted nothing more specific than the generic wording. Set it
		// wherever an arm exists for the sake of its message rather than for
		// the rejection itself.
		wantMsg string
	}{
		{name: "empty", value: ""},
		{name: "dot", value: ".", wantMsg: "that is a directory reference, not an interface name"},
		{name: "dot dot", value: "..", wantMsg: "that is a directory reference, not an interface name"},
		{name: "too long by one", value: "0123456789012345"},
		{name: "embedded space splits an argv", value: attackNMProfileName},
		{name: "path traversal escapes NMSTATEDIR", value: attackPathTraversal},
		{name: "newline and CSI forge a plan row", value: attackPlanRowInjection},
		{name: "shell metacharacters", value: "eth0;reboot"},
		{name: "slash", value: "net/eth0"},
		{name: "leading dash looks like a flag", value: "--help"},
		{name: "non-ascii rune", value: "eth\u00f80"},
		{name: "nul-ish control byte", value: "eth0\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStoredInterfaceName("bridge name", tt.value)
			if err == nil {
				t.Fatalf("validateStoredInterfaceName(%q) = nil, want an error", tt.value)
			}
			msg := err.Error()
			if tt.wantMsg != "" && !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("error %q does not contain %q: the arm that produces that message is no longer reached, and the generic one has taken over", msg, tt.wantMsg)
			}
			if !strings.Contains(msg, "bridge name") {
				t.Errorf("error does not name the field it came from: %q", msg)
			}
			if !strings.Contains(msg, "stored configuration") {
				t.Errorf("error does not say the value came from stored configuration: %q", msg)
			}
			// The rejected value is quoted with %q, so reporting it cannot
			// itself deliver the escape sequence that got it rejected.
			for _, r := range msg {
				if r < 0x20 || r == 0x7f {
					t.Errorf("error message carries raw control byte %#x: %q", r, msg)
				}
			}
		})
	}
}

// "--help" is rejected above for its leading dash, so the reader does not have
// to reason about whether nmcli would treat a stored name as a flag. This
// pins that the value is still reported, since an error that hides what it
// rejected sends the user looking in the wrong file.
func TestValidateStoredInterfaceNameReportsTheValue(t *testing.T) {
	err := validateStoredInterfaceName("tap name", attackNMProfileName)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), attackNMProfileName) {
		t.Errorf("error %q does not contain the rejected value %q", err, attackNMProfileName)
	}
	if !strings.Contains(err.Error(), "tap name") {
		t.Errorf("error %q does not name the tap name field", err)
	}
}

// The traversal case is the reason the validator runs before
// sharedLeaseFilePath rather than after it: the join cannot be made safe on
// its own, because cleaning the result is exactly what performs the escape.
func TestSharedLeaseFilePathEscapesWithoutValidation(t *testing.T) {
	got := sharedLeaseFilePath(attackPathTraversal)
	if strings.HasPrefix(got, nmStateDir+"/") {
		t.Fatalf("sharedLeaseFilePath(%q) = %q, expected it to escape %q -- if this now stays inside the directory, the validator is no longer the only thing keeping it there",
			attackPathTraversal, got, nmStateDir)
	}
	if got != "/var/lib/etc/shadow.leases" {
		t.Errorf("sharedLeaseFilePath(%q) = %q, want %q", attackPathTraversal, got, "/var/lib/etc/shadow.leases")
	}
	// Every name the validator does accept stays inside NMSTATEDIR.
	for _, name := range []string{DefaultBridgeName, DefaultTapName, "eth0"} {
		if err := validateStoredInterfaceName("bridge name", name); err != nil {
			t.Fatalf("fixture %q is not accepted: %v", name, err)
		}
		p := sharedLeaseFilePath(name)
		if filepath.Dir(p) != nmStateDir {
			t.Errorf("sharedLeaseFilePath(%q) = %q, outside %q", name, p, nmStateDir)
		}
	}
}

// parseBridgeSlave lives in this untagged file, so its test belongs here
// too: network_linux_test.go inherits the _linux.go constraint and would
// run this on the ubuntu CI leg only, for logic that compiles on both.
func TestParseBridgeSlave(t *testing.T) {
	const bridge = DefaultBridgeName
	tests := []struct {
		name string
		out  string
		tap  string
		want string
	}{
		{
			name: "physical slave after the tap",
			out: ipLinkLine(3, DefaultTapName, bridge) + "\n" +
				ipLinkLine(4, "enp0s31f6", bridge) + "\n",
			tap:  DefaultTapName,
			want: "enp0s31f6",
		},
		{
			// The shared-mode shape: the bridge's only port is the tap, and
			// there is nothing to put back on the host.
			name: "only the tap",
			out:  ipLinkLine(3, DefaultTapName, bridge) + "\n",
			tap:  DefaultTapName,
			want: "",
		},
		{
			name: "empty output",
			out:  "",
			tap:  DefaultTapName,
			want: "",
		},
		{
			name: "malformed output",
			out:  "\n\n   \ngarbage\n4:\n",
			tap:  DefaultTapName,
			want: "",
		},
		{
			// The regression the substring match caused: this host NIC is
			// not the tap, and skipping it leaves the host with no active
			// connection after a teardown and nothing said about it.
			name: "host NIC whose name contains tap",
			out: ipLinkLine(3, DefaultTapName, bridge) + "\n" +
				ipLinkLine(4, "captap0", bridge) + "\n",
			tap:  DefaultTapName,
			want: "captap0",
		},
		{
			name: "configured tap is skipped",
			out: ipLinkLine(3, "kltap0", bridge) + "\n" +
				ipLinkLine(4, "eth0", bridge) + "\n",
			tap:  "kltap0",
			want: "eth0",
		},
		{
			// A tap name edited in state.json after the tap was created must
			// not turn the real tap into a physical slave.
			name: "default tap is skipped even when another one is configured",
			out:  ipLinkLine(3, DefaultTapName, bridge) + "\n",
			tap:  "kltap0",
			want: "",
		},
		{
			name: "trailing newline only",
			out:  "\n",
			tap:  DefaultTapName,
			want: "",
		},
		{
			// `ip` renders a device with a link-layer parent as
			// "<name>@<parent>", and only the part before the '@' is a
			// device name: the teardown hands this straight to `nmcli device
			// connect`, which fails on "eth0.100@eth0".
			name: "a VLAN slave is named as a device",
			out: ipLinkLine(3, DefaultTapName, bridge) + "\n" +
				ipLinkLine(4, "eth0.100@eth0", bridge) + "\n",
			tap:  DefaultTapName,
			want: "eth0.100",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseBridgeSlave(tt.out, tt.tap); got != tt.want {
				t.Errorf("parseBridgeSlave(..., %q) = %q, want %q\nfrom:\n%s", tt.tap, got, tt.want, tt.out)
			}
		})
	}
}

// ipLinkLine is one line of `ip -o link show`, continuation and all: the "\\"
// before the link/ether half is what -o substitutes for the newline, and it
// is there so the parser is fed the real shape rather than a tidied one.
func ipLinkLine(index int, iface, bridge string) string {
	return fmt.Sprintf(
		"%d: %s: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue master %s state UP mode DEFAULT group default qlen 1000\\    link/ether 02:00:00:00:00:%02x brd ff:ff:ff:ff:ff:ff",
		index, iface, bridge, index)
}

// parseBridgePorts is parseBridgeSlave's sibling and excludes nothing, which
// is the whole of the difference: the name the shared path's assertion would
// have to exclude to reuse the other one is st.Network.TapName, out of the
// same 0644 state.json the assertion defends against.
func TestParseBridgePorts(t *testing.T) {
	const bridge = DefaultBridgeName
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "no output at all",
			out:  "",
			want: nil,
		},
		{
			// The shape the two pre-activation checks require: a bridge with
			// nothing on it reports nothing, and only then do they pass.
			name: "a bridge with no ports",
			out:  "\n",
			want: nil,
		},
		{
			// The tap is a port like any other here. Before the tap is
			// activated its caller expects no ports at all, so this function
			// may not be the thing that decides the tap is special.
			name: "the tap is not special",
			out:  ipLinkLine(3, DefaultTapName, bridge) + "\n",
			want: []string{DefaultTapName},
		},
		{
			name: "every port, in the order the kernel printed them",
			out: ipLinkLine(3, DefaultTapName, bridge) + "\n" +
				ipLinkLine(4, "eth0", bridge) + "\n" +
				ipLinkLine(5, "wlan0", bridge) + "\n",
			want: []string{DefaultTapName, "eth0", "wlan0"},
		},
		{
			name: "malformed lines are skipped",
			out:  "\n\n   \ngarbage\n4:\n" + ipLinkLine(5, "eth0", bridge) + "\n",
			want: []string{"eth0"},
		},
		{
			name: "a VLAN and a veth port are named as devices",
			out: ipLinkLine(3, "eth0.100@eth0", bridge) + "\n" +
				ipLinkLine(4, "veth7a1b@if12", bridge) + "\n",
			want: []string{"eth0.100", "veth7a1b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBridgePorts(tt.out)
			if !slices.Equal(got, tt.want) {
				t.Errorf("parseBridgePorts() = %q, want %q\nfrom:\n%s", got, tt.want, tt.out)
			}
		})
	}
}

// The predicate the three assertions are built on. An empty expected list is
// the one the two pre-activation checks pass, and it is what removes the
// untrusted tap name from the decision entirely.
func TestUnexpectedBridgePorts(t *testing.T) {
	tests := []struct {
		name     string
		ports    []string
		expected []string
		want     []string
	}{
		{
			name:  "nothing on the bridge, nothing expected",
			ports: nil,
			want:  nil,
		},
		{
			name:  "the tap alone is unexpected before it is activated",
			ports: []string{DefaultTapName},
			want:  []string{DefaultTapName},
		},
		{
			name:     "the tap alone is expected once it is",
			ports:    []string{DefaultTapName},
			expected: []string{DefaultTapName},
			want:     nil,
		},
		{
			name:     "a host NIC beside the tap is not",
			ports:    []string{DefaultTapName, "eth0"},
			expected: []string{DefaultTapName},
			want:     []string{"eth0"},
		},
		{
			name:     "every unexpected port is returned, in order",
			ports:    []string{"eth0", DefaultTapName, "wlan0"},
			expected: []string{DefaultTapName},
			want:     []string{"eth0", "wlan0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unexpectedBridgePorts(tt.ports, tt.expected)
			if !slices.Equal(got, tt.want) {
				t.Errorf("unexpectedBridgePorts(%q, %q) = %q, want %q", tt.ports, tt.expected, got, tt.want)
			}
		})
	}
}

// Interface names reach the terminal through these errors, and they passed no
// validator on the way: dev_valid_name() bars only NUL, '/', ':' and
// whitespace, so the kernel accepts a name with a raw ESC or a U+202E in it.
func TestQuoteNames(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{name: "nothing", names: nil, want: ""},
		{name: "one name", names: []string{"eth0"}, want: `"eth0"`},
		{name: "several", names: []string{"eth0", "wlan0"}, want: `"eth0", "wlan0"`},
		{
			name:  "a control byte is escaped",
			names: []string{"eth0" + "\x1b" + "[2K"},
			want:  `"eth0\x1b[2K"`,
		},
		{
			name:  "a direction override is escaped",
			names: []string{"eth0" + "\u202e"},
			want:  `"eth0\u202e"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quoteNames(tt.names)
			if got != tt.want {
				t.Errorf("quoteNames(%q) = %s, want %s", tt.names, got, tt.want)
			}
			for _, r := range got {
				if !strconv.IsPrint(r) {
					t.Errorf("quoteNames(%q) returned an unprintable rune %U", tt.names, r)
				}
			}
		})
	}
}

// renderArgv is the render boundary for the command line inside a failed
// root command's error, and it exists because %q on the CALLER's copy of an
// interface name leaves the copy inside the wrapped error raw. Both copies
// end up in one string on one terminal.
//
// The rule is planValue's: a word whose runes are all printable is left
// alone, so an ordinary failure still reads back as the command that failed.
func TestRenderArgv(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "nothing", argv: nil, want: ""},
		{
			name: "an ordinary teardown command is untouched",
			argv: []string{"nmcli", "connection", "delete", "kairoslab0"},
			want: "nmcli connection delete kairoslab0",
		},
		{
			name: "an ordinary link delete is untouched",
			argv: []string{"ip", "link", "delete", "kairoslab-tap0"},
			want: "ip link delete kairoslab-tap0",
		},
		{
			// The name the teardown reconnects: straight out of `ip -o link
			// show master`, through no validator.
			name: "a control byte in an interface name",
			argv: []string{"nmcli", "device", "connect", "eth0" + "\x1b" + "[2K"},
			want: `nmcli device connect "eth0\x1b[2K"`,
		},
		{
			name: "a newline in an interface name",
			argv: []string{"nmcli", "device", "connect", "eth0\nwlan0"},
			want: `nmcli device connect "eth0\nwlan0"`,
		},
		{
			name: "a direction override in an interface name",
			argv: []string{"nmcli", "device", "connect", "eth0" + "\u202e"},
			want: `nmcli device connect "eth0\u202e"`,
		},
		{
			// 0x9b is the 8-bit CSI and is not valid UTF-8 on its own, so a
			// range loop would decode it as U+FFFD -- which is printable.
			name: "an invalid UTF-8 byte",
			argv: []string{"nmcli", "device", "connect", "eth0" + "\x9b" + "2K"},
			want: `nmcli device connect "eth0\x9b2K"`,
		},
		{
			// Only the words that need it, and only the ones that do: a
			// quoted word beside untouched ones is what keeps the line
			// readable.
			name: "a space keeps the word one word",
			argv: []string{"nmcli", "connection", "delete", "Wired connection 1"},
			want: `nmcli connection delete "Wired connection 1"`,
		},
		{
			// An unquoted empty word would vanish into the join and leave a
			// shorter argv than the one that failed.
			name: "an empty word is still a word",
			argv: []string{"ip", "link", "delete", ""},
			want: `ip link delete ""`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderArgv(tt.argv)
			if got != tt.want {
				t.Errorf("renderArgv(%q) = %s, want %s", tt.argv, got, tt.want)
			}
			for _, r := range got {
				if !strconv.IsPrint(r) {
					t.Errorf("renderArgv(%q) left the unprintable rune %U on the line: %q", tt.argv, r, got)
				}
			}
		})
	}
}

// A failing probe's stderr is quoted into the refusal, because it is what
// tells the causes that refusal lists apart. It gets one line and a cap; the
// rest of the message has to stay readable beside it.
func TestFirstLine(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{name: "empty", in: "", maxLen: 20, want: ""},
		{
			name:   "what busybox says to `show master`",
			in:     "ip: either \"dev\" is duplicate, or \"br0\" is garbage\n",
			maxLen: 200,
			want:   `ip: either "dev" is duplicate, or "br0" is garbage`,
		},
		{
			name:   "only the first line",
			in:     "first\nsecond\nthird\n",
			maxLen: 200,
			want:   "first",
		},
		{
			name:   "cut to the cap",
			in:     strings.Repeat("a", 50),
			maxLen: 10,
			want:   strings.Repeat("a", 10),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.in, tt.maxLen); got != tt.want {
				t.Errorf("firstLine(%q, %d) = %q, want %q", tt.in, tt.maxLen, got, tt.want)
			}
		})
	}
}
