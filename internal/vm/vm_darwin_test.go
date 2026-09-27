//go:build darwin

package vm

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

func macOSBridgeConfig(iface string) StartConfig {
	return StartConfig{
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "bridged",
		MacOSBiosPath: "/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
		BridgeIface:   iface,
	}
}

// The old code silently substituted en0 here, which is how a VM ended up
// bridged onto an unplugged port (kairos-io/kairos#4431).
func TestBuildMacOSRejectsUnresolvedBridgeIface(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	_, args, err := buildMacOS(macOSBridgeConfig(""))
	if err == nil {
		t.Fatalf("expected an error for an unresolved bridge interface, got args: %v", args)
	}
	if !strings.Contains(err.Error(), "bridge interface") {
		t.Errorf("error %q should name the missing bridge interface", err)
	}
}

func TestBuildMacOSUsesResolvedBridgeIface(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	_, args, err := buildMacOS(macOSBridgeConfig("en1"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "vmnet-bridged,id=net0,ifname=en1") {
		t.Fatalf("expected the resolved interface in args: %s", joined)
	}
	if strings.Contains(joined, "ifname=en0") {
		t.Fatalf("en0 should no longer be substituted: %s", joined)
	}
}

// User mode needs no interface at all, so it must not be caught by the check.
func TestBuildMacOSUserModeNeedsNoBridgeIface(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	cfg := macOSBridgeConfig("")
	cfg.NetworkMode = "user"
	_, args, err := buildMacOS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "hostfwd=tcp::2222-:22") {
		t.Fatalf("expected the user-mode port forwards: %v", args)
	}
}

func TestBuildMacOSSharedModeUsesVmnetShared(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	cfg := macOSBridgeConfig("")
	cfg.NetworkMode = "shared"
	cfg.MACAddress = testMACAddress
	_, args, err := buildMacOS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(t, args, "-netdev"); got != "vmnet-shared,id=net0" {
		t.Errorf("-netdev = %q, want vmnet-shared,id=net0", got)
	}
	wantDevice := "virtio-net-pci,netdev=net0,mac=" + testMACAddress
	if got := nicDeviceArg(t, args); got != wantDevice {
		t.Errorf("NIC device = %q, want %q", got, wantDevice)
	}
	assertOneNIC(t, args)
}

// vmnet-shared attaches to no host interface: NetdevVmnetSharedOptions has no
// ifname member, and QEMU's opts visitor rejects a leftover key with "Invalid
// parameter", so an ifname here aborts the VM at startup rather than being
// ignored. The assertion is deliberately over the whole command line.
func TestBuildMacOSSharedModePassesNoIfname(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	cfg := macOSBridgeConfig("en1")
	cfg.NetworkMode = "shared"
	_, args, err := buildMacOS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Assert the backend positively first. Checking only for the ABSENCE of
	// ifname= passes vacuously on an empty arg list, so a mutation returning
	// ("", nil, nil) would leave this test green and rely on its sibling.
	if got := argAfter(t, args, "-netdev"); got != "vmnet-shared,id=net0" {
		t.Fatalf("-netdev = %q, want vmnet-shared,id=net0", got)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "ifname=") {
		t.Fatalf("shared mode must pass no ifname: QEMU has no ifname option for "+
			"vmnet-shared and aborts at startup on the unknown key, args: %s", joined)
	}
}

// The bridged-only guard must not catch the modes that need no interface.
func TestBuildMacOSSharedAndUserNeedNoBridgeIface(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	for _, mode := range []string{"shared", "user"} {
		t.Run(mode, func(t *testing.T) {
			cfg := macOSBridgeConfig("")
			cfg.NetworkMode = mode
			if _, _, err := buildMacOS(cfg); err != nil {
				t.Errorf("%s mode should not need a bridge interface: %v", mode, err)
			}
		})
	}
}

func TestBuildMacOSBridgedCarriesMAC(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	cfg := macOSBridgeConfig("en1")
	cfg.MACAddress = testMACAddress
	_, args, err := buildMacOS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(t, args, "-netdev"); got != "vmnet-bridged,id=net0,ifname=en1" {
		t.Errorf("-netdev = %q, want vmnet-bridged,id=net0,ifname=en1", got)
	}
	wantDevice := "virtio-net-pci,netdev=net0,mac=" + testMACAddress
	if got := nicDeviceArg(t, args); got != wantDevice {
		t.Errorf("NIC device = %q, want %q", got, wantDevice)
	}
	assertOneNIC(t, args)
}

// Without the guest-agent socket the IP resolver M4 builds will have no QGA
// source on macOS at all, so this has to match what buildLinux emits. (On
// macOS that source will be best-effort even when present: bridged mode runs
// QEMU under sudo, so the socket lands root-owned -- see the comment in
// buildMacOS.)
func TestBuildMacOSIncludesGuestAgent(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	_, args, err := buildMacOS(macOSBridgeConfig("en1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(t, args, "-chardev"); got != "socket,path=/tmp/kairos.sock,server=on,wait=off,id=qga0" {
		t.Errorf("-chardev = %q, want the qga0 socket backend", got)
	}
	// Exact values, not substrings of the joined command line: "virtio-serial"
	// is a prefix of "virtio-serial-pci", so a Contains check cannot tell the
	// two spellings apart. The alias resolution is deliberate -- qdev resolves
	// virtio-serial to virtio-serial-pci on QEMU_ARCH_ARM -- so what is pinned
	// here is that buildMacOS spells it the way buildLinux does, for parity,
	// not the name QEMU resolves it to.
	devices := deviceArgs(args)
	for _, want := range []string{
		"virtio-serial",
		"virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
	} {
		if !slices.Contains(devices, want) {
			t.Errorf("expected a -device with the exact value %q, got devices %v", want, devices)
		}
	}
}

// The macOS counterpart of TestBuildLinuxEmptyMACOmitsMACEntirely: the
// graceful degradation has to be symmetric between the two builders.
func TestBuildMacOSEmptyMACOmitsMACEntirely(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	// shared attaches to no host interface, so BridgeIface stays empty:
	// StartConfig documents it as meaningful for bridged mode only.
	cfg := macOSBridgeConfig("")
	cfg.NetworkMode = "shared"
	_, args, err := buildMacOS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if device := nicDeviceArg(t, args); device != "virtio-net-pci,netdev=net0" {
		t.Errorf("NIC device = %q, want the bare device with no MAC suffix", device)
	}
	if strings.Contains(strings.Join(args, " "), "mac=") {
		t.Errorf("an empty MACAddress must not put mac= on the command line: %v", args)
	}
	assertOneNIC(t, args)
}

func TestBuildMacOSNetdevPerNetworkMode(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	const userNetdev = "user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080"
	for _, tc := range []struct {
		name       string
		mode       string
		wantNetdev string
	}{
		{"shared", "shared", "vmnet-shared,id=net0"},
		{"bridged", "bridged", "vmnet-bridged,id=net0,ifname=en1"},
		{"user", "user", userNetdev},
		// The default: arm is defence in depth: an unvalidated mode must still
		// leave the guest a working NIC rather than no -netdev at all, so
		// turning default: into case "user": has to fail here.
		{"empty mode falls back to user networking", "", userNetdev},
		{"unknown mode falls back to user networking", "bogus", userNetdev},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Subtests, not a bare loop: argAfter and nicDeviceArg abort with
			// t.Fatalf, which in a bare loop would stop the later rows from
			// running at all.
			cfg := macOSBridgeConfig("en1")
			cfg.NetworkMode = tc.mode
			cfg.MACAddress = testMACAddress
			_, args, err := buildMacOS(cfg)
			if err != nil {
				t.Fatalf("%q mode: %v", tc.mode, err)
			}
			if got := argAfter(t, args, "-netdev"); got != tc.wantNetdev {
				t.Errorf("%q mode: -netdev = %q, want %q", tc.mode, got, tc.wantNetdev)
			}
			wantDevice := "virtio-net-pci,netdev=net0,mac=" + testMACAddress
			if got := nicDeviceArg(t, args); got != wantDevice {
				t.Errorf("%q mode: NIC device = %q, want %q", tc.mode, got, wantDevice)
			}
			assertOneNIC(t, args)
		})
	}
}

func TestBuildMacOSRejectsMalformedMAC(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	// Comma is QEMU's option separator, so a corrupted or hand-edited MAC in
	// state.json would otherwise inject extra device properties.
	const bad = "52:54:00:12:34:56,romfile=/tmp/evil.rom"
	cfg := macOSBridgeConfig("en1")
	cfg.MACAddress = bad
	_, args, err := buildMacOS(cfg)
	if err == nil {
		t.Fatalf("expected an error for a malformed MAC, got args: %v", args)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q should name the offending value %q", err, bad)
	}
}

func TestBuildMacOSPadsStrippedMAC(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("macOS support is Apple Silicon only")
	}
	// macOS sources print MACs zero-stripped; QEMU wants them padded.
	cfg := macOSBridgeConfig("en1")
	cfg.MACAddress = "52:54:0:a:4:f"
	_, args, err := buildMacOS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const want = "virtio-net-pci,netdev=net0,mac=52:54:00:0a:04:0f"
	if got := nicDeviceArg(t, args); got != want {
		t.Errorf("NIC device = %q, want %q", got, want)
	}
}
