package vm

import (
	"strings"
	"testing"
)

// Sample output taken from the macOS host in kairos-io/kairos#4431: en0 is the
// built-in Ethernet with no cable, en1 is the Wi-Fi radio holding the default
// route.
const (
	routeGetDefaultOut = `   route to: default
destination: default
       mask: default
    gateway: 192.168.68.1
  interface: en1
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1500         0
`

	ifconfigInactiveOut = `en0: flags=8963<UP,BROADCAST,SMART,RUNNING,PROMISC,SIMPLEX,MULTICAST> mtu 1500
	options=6460<TSO4,TSO6,CHANNEL_IO,PARTIAL_CSUM,ZEROINVERT_CSUM>
	ether 5c:e9:1e:c0:6f:4c
	media: autoselect (none)
	status: inactive
`

	ifconfigActiveOut = `en1: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	ether 3c:22:fb:1d:2e:aa
	inet 192.168.68.61 netmask 0xffffff00 broadcast 192.168.68.255
	media: autoselect
	status: active
`

	ifconfigLoopbackOut = `lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384
	inet 127.0.0.1 netmask 0xff000000
`

	hardwarePortsOut = `Hardware Port: Ethernet
Device: en0
Ethernet Address: 5c:e9:1e:c0:6f:4c

Hardware Port: Wi-Fi
Device: en1
Ethernet Address: 3c:22:fb:1d:2e:aa

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: 36:1a:8e:71:04:80

VLAN Configurations
===
`
)

func TestParseDarwinDefaultRouteIface(t *testing.T) {
	if got := parseDarwinDefaultRouteIface(routeGetDefaultOut); got != "en1" {
		t.Fatalf("default route interface = %q, want en1", got)
	}
	// `route -n get default` prints this when there is no default route.
	if got := parseDarwinDefaultRouteIface("   route to: default\n"); got != "" {
		t.Fatalf("interface without a default route = %q, want empty", got)
	}
}

func TestParseDarwinIfaceStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"unplugged ethernet", ifconfigInactiveOut, "inactive"},
		{"associated wifi", ifconfigActiveOut, "active"},
		{"loopback reports none", ifconfigLoopbackOut, ""},
		{"empty output", "", ""},
	} {
		if got := parseDarwinIfaceStatus(tc.out); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseDarwinIfaceList(t *testing.T) {
	got := parseDarwinIfaceList("lo0 gif0 stf0 en0 en1 bridge0 ap1 awdl0 llw0 utun0\n")
	want := []string{"lo0", "gif0", "stf0", "en0", "en1", "bridge0", "ap1", "awdl0", "llw0", "utun0"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("interface list = %v, want %v", got, want)
	}
}

func TestIsDarwinVirtualInterface(t *testing.T) {
	// vmnet cannot bridge onto any of these, and bridge100/vmenet0 are the
	// devices a previous kairos-lab run leaves behind.
	for _, iface := range []string{"lo0", "gif0", "stf0", "bridge100", "vmenet0", "awdl0", "llw0", "utun3", "ap1"} {
		if !isDarwinVirtualInterface(iface) {
			t.Errorf("%s should be treated as virtual", iface)
		}
	}
	for _, iface := range []string{"en0", "en1", "en10"} {
		if isDarwinVirtualInterface(iface) {
			t.Errorf("%s should be treated as physical", iface)
		}
	}
}

func TestParseDarwinHardwarePorts(t *testing.T) {
	ports := parseDarwinHardwarePorts(hardwarePortsOut)
	if ports["en1"] != "Wi-Fi" {
		t.Errorf("en1 port = %q, want Wi-Fi", ports["en1"])
	}
	if ports["en0"] != "Ethernet" {
		t.Errorf("en0 port = %q, want Ethernet", ports["en0"])
	}
	if !isWiFiPort(ports["en1"]) {
		t.Error("en1 should be recognised as Wi-Fi")
	}
	if isWiFiPort(ports["en0"]) {
		t.Error("en0 should not be recognised as Wi-Fi")
	}
	// Older macOS releases name the same port AirPort.
	if !isWiFiPort("AirPort") {
		t.Error("AirPort should be recognised as Wi-Fi")
	}
}

func TestValidateBridgeIface(t *testing.T) {
	hostIfaces := []string{"lo0", "en0", "en1", "awdl0", "bridge100"}

	if err := validateBridgeIface("en1", "active", hostIfaces, []string{"en1"}, FlagBridgeControls); err != nil {
		t.Fatalf("an interface with a link should validate: %v", err)
	}

	// The exact failure from the issue: en0 is up and RUNNING but has no
	// cable, so the guest's DHCP requests die on the bridge.
	err := validateBridgeIface("en0", "inactive", hostIfaces, []string{"en1"}, FlagBridgeControls)
	if err == nil {
		t.Fatal("expected an error for an inactive interface")
	}
	for _, want := range []string{"en0", "inactive", "en1", "-network user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}

	err = validateBridgeIface("en0", "", hostIfaces, nil, FlagBridgeControls)
	if err == nil {
		t.Fatal("expected an error for an interface with no link status")
	}
	if !strings.Contains(err.Error(), "no host interface currently has a link") {
		t.Errorf("error %q should say no candidate has a link", err)
	}
}

// awdl0 is AirDrop and reports "active" on a real Mac, so the link check alone
// lets an explicit -bridge-if walk past the filter DetectBridgeIfaceCandidates
// applies (kairos-io/kairos#4649).
func TestValidateBridgeIfaceRejectsVirtual(t *testing.T) {
	hostIfaces := []string{"lo0", "en0", "en1", "awdl0", "bridge100"}

	for _, iface := range []string{"awdl0", "bridge100", "lo0"} {
		err := validateBridgeIface(iface, "active", hostIfaces, []string{"en1"}, FlagBridgeControls)
		if err == nil {
			t.Fatalf("%s is virtual and should not be accepted as a bridge interface", iface)
		}
		for _, want := range []string{iface, "virtual device", "en1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q", err, want)
			}
		}
	}
}

// `ifconfig en9` fails for a name that is not on the host, so its status is ""
// and used to be reported as a link problem (kairos-io/kairos#4649).
func TestValidateBridgeIfaceRejectsUnknownName(t *testing.T) {
	hostIfaces := []string{"lo0", "en0", "en1", "awdl0"}

	err := validateBridgeIface("en9", "", hostIfaces, []string{"en1"}, FlagBridgeControls)
	if err == nil {
		t.Fatal("expected an error for an interface that does not exist")
	}
	if !strings.Contains(err.Error(), "no such interface en9") {
		t.Errorf("error %q should say the interface does not exist", err)
	}
	if strings.Contains(err.Error(), "link status") {
		t.Errorf("error %q should not diagnose a link problem for a typo", err)
	}

	// `ifconfig -l` itself can fail, and then every name is unknown. Fall
	// back to the link diagnosis rather than calling a real interface a typo.
	err = validateBridgeIface("en0", "inactive", nil, []string{"en1"}, FlagBridgeControls)
	if err == nil {
		t.Fatal("expected an error for an inactive interface")
	}
	if strings.Contains(err.Error(), "no such interface") {
		t.Errorf("error %q should not claim en0 is missing when the host list is empty", err)
	}
}

func TestWiFiBridgeWarning(t *testing.T) {
	warning := wifiBridgeWarning("en1")
	if !strings.Contains(warning, "en1") || !strings.Contains(warning, "Wi-Fi") {
		t.Fatalf("warning should name the interface and Wi-Fi: %q", warning)
	}
}

// The empty name was unreachable only because all three callers happened to
// guard against it, and the message it produced named no interface and carried
// a double space: "no such interface  on this host (...)". The guard belongs in
// the validator (kairos-io/kairos#4649).
func TestValidateBridgeIfaceRejectsEmptyName(t *testing.T) {
	hostIfaces := []string{"lo0", "en0", "en1"}

	err := validateBridgeIface("", "", hostIfaces, []string{"en1"}, FlagBridgeControls)
	if err == nil {
		t.Fatal("expected an error for an empty interface name")
	}
	if strings.Contains(err.Error(), "no such interface") {
		t.Errorf("error %q should not read as a missing named interface", err)
	}
	if strings.Contains(err.Error(), "  ") {
		t.Errorf("error %q has a double space where the name should be", err)
	}
	// It still has to say what the user can do next.
	if !strings.Contains(err.Error(), "en1") {
		t.Errorf("error %q should name the interfaces a bridge could use", err)
	}
}

// The advice tail has to match where the user is standing. Inside the config
// review -bridge-if and -network are already spent, and telling someone to
// pass a flag to a command that is mid-run is the wrong instruction
// (kairos-io/kairos#4649).
func TestValidateBridgeIfaceAdviceFollowsTheCaller(t *testing.T) {
	hostIfaces := []string{"lo0", "en0", "en1"}

	flagErr := validateBridgeIface("en0", "inactive", hostIfaces, []string{"en1"}, FlagBridgeControls)
	if flagErr == nil {
		t.Fatal("expected an error for an inactive interface")
	}
	for _, want := range []string{"-bridge-if", "-network user"} {
		if !strings.Contains(flagErr.Error(), want) {
			t.Errorf("command-line error %q should mention %q", flagErr, want)
		}
	}

	reviewErr := validateBridgeIface("en0", "inactive", hostIfaces, []string{"en1"}, ReviewBridgeControls)
	if reviewErr == nil {
		t.Fatal("expected an error for an inactive interface")
	}
	for _, want := range []string{"option 8", "option 7"} {
		if !strings.Contains(reviewErr.Error(), want) {
			t.Errorf("config-review error %q should mention %q", reviewErr, want)
		}
	}
	for _, unwanted := range []string{"-bridge-if", "-network user"} {
		if strings.Contains(reviewErr.Error(), unwanted) {
			t.Errorf("config-review error %q should not send the user to %q", reviewErr, unwanted)
		}
	}

	// Both wordings still have to carry the diagnosis itself.
	for _, err := range []error{flagErr, reviewErr} {
		for _, want := range []string{"en0", "inactive", "en1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q", err, want)
			}
		}
	}
}
