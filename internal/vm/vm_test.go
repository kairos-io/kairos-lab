package vm

import (
	"runtime"
	"strings"
	"testing"
)

func TestBuildLinuxCommandIncludesTapInBridgeMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, args, err := buildLinux(StartConfig{
		ISOPath:       "/tmp/kairos.iso",
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "bridged",
		LinuxTapName:  "kairoslab-tap0",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ifname=kairoslab-tap0") {
		t.Fatalf("expected tap interface in args: %s", joined)
	}
}

func TestBuildLinuxCommandFailsWithoutTapInBridgeMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, _, err := buildLinux(StartConfig{NetworkMode: "bridged"})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestBuildLinuxCommandSerialDisplayUsesNographic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, args, err := buildLinux(StartConfig{
		ISOPath:       "/tmp/kairos.iso",
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "user",
		DisplayMode:   "serial",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-nographic") {
		t.Fatalf("expected -nographic for serial display mode: %s", joined)
	}
}

func TestBuildLinuxCommandWindowDisplayOmitsNographic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, args, err := buildLinux(StartConfig{
		ISOPath:       "/tmp/kairos.iso",
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "user",
		DisplayMode:   "window",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-nographic") {
		t.Fatalf("did not expect -nographic for window display mode: %s", joined)
	}
	if !strings.Contains(joined, "-display default") {
		t.Fatalf("expected explicit display backend for window display mode: %s", joined)
	}
	if runtime.GOARCH == "arm64" && !strings.Contains(joined, "virtio-gpu-pci") {
		t.Fatalf("expected virtio-gpu-pci for arm64 window display mode: %s", joined)
	}
}

func TestBuildLinuxNetdevPerNetworkMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	// shared and bridged are the same command line on Linux: both hand the
	// guest a tap on a NetworkManager bridge, and only the bridge's own IPv4
	// method differs, which is not configured here.
	const tapNetdev = "tap,id=net0,ifname=kairoslab-tap0,script=no,downscript=no"
	const userNetdev = "user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080"
	for _, tc := range []struct {
		name       string
		mode       string
		wantNetdev string
	}{
		{"shared", "shared", tapNetdev},
		{"bridged", "bridged", tapNetdev},
		{"user", "user", userNetdev},
		// The default: arm of the switch is defence in depth, not dead code:
		// an unvalidated mode must still leave the guest a working NIC rather
		// than no -netdev and no interface at all. Turning default: into
		// case "user": has to fail here.
		{"empty mode falls back to user networking", "", userNetdev},
		{"unknown mode falls back to user networking", "bogus", userNetdev},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Subtests, not a bare loop: argAfter and nicDeviceArg abort with
			// t.Fatalf, which in a bare loop would stop the later rows from
			// running at all and hide whatever they would have caught.
			_, args, err := buildLinux(StartConfig{
				ISOPath:       "/tmp/kairos.iso",
				DiskPath:      "/tmp/kairos.qcow2",
				QGASocketPath: "/tmp/kairos.sock",
				CPUs:          2,
				MemoryMB:      4096,
				NetworkMode:   tc.mode,
				LinuxTapName:  "kairoslab-tap0",
				MACAddress:    testMACAddress,
			})
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
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "mac="+testMACAddress) {
				t.Errorf("%q mode: expected the configured MAC in args: %s", tc.mode, joined)
			}
			assertOneNIC(t, args)
		})
	}
}

func TestBuildLinuxSharedModeRequiresTapAndNamesItself(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, _, err := buildLinux(StartConfig{NetworkMode: "shared"})
	if err == nil {
		t.Fatal("expected an error for shared mode with no tap name")
	}
	// The error used to hardcode "bridged", which sends someone starting a
	// shared VM looking at bridged configuration they never touched.
	if !strings.Contains(err.Error(), "shared") {
		t.Errorf("error %q should name the shared mode", err)
	}
	if strings.Contains(err.Error(), "bridged") {
		t.Errorf("error %q should not name bridged for a shared start", err)
	}
}

func TestBuildLinuxEmptyMACOmitsMACEntirely(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	// A caller that never sets MACAddress must degrade to QEMU's own default
	// address, not emit a dangling "mac=" that QEMU refuses to parse.
	_, args, err := buildLinux(StartConfig{
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "shared",
		LinuxTapName:  "kairoslab-tap0",
	})
	if err != nil {
		t.Fatal(err)
	}
	device := nicDeviceArg(t, args)
	if device != "virtio-net-pci,netdev=net0" {
		t.Errorf("NIC device = %q, want the bare device with no MAC suffix", device)
	}
	if strings.Contains(strings.Join(args, " "), "mac=") {
		t.Errorf("an empty MACAddress must not put mac= on the command line: %v", args)
	}
	assertOneNIC(t, args)
}

func TestBuildLinuxRejectsMalformedMAC(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	// From M5 this value comes out of the user's state.json, so a corrupted or
	// hand-edited entry must name itself here rather than reach QEMU.
	const bad = "52:54:00:12:34:56,romfile=/tmp/evil.rom"
	_, args, err := buildLinux(StartConfig{
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "shared",
		LinuxTapName:  "kairoslab-tap0",
		MACAddress:    bad,
	})
	if err == nil {
		t.Fatalf("expected an error for a malformed MAC, got args: %v", args)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q should name the offending value %q", err, bad)
	}
}

func TestBuildLinuxPadsStrippedMAC(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	// A MAC read back off macOS is zero-stripped. QEMU's parser wants the
	// padded spelling, so the command line must carry the padded form even
	// when the stored value is stripped.
	_, args, err := buildLinux(StartConfig{
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "user",
		MACAddress:    "52:54:0:a:4:f",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "virtio-net-pci,netdev=net0,mac=52:54:00:0a:04:0f"
	if got := nicDeviceArg(t, args); got != want {
		t.Errorf("NIC device = %q, want %q", got, want)
	}
}

// netDeviceArg is host-independent, so this runs on every OS.
func TestNetDeviceArg(t *testing.T) {
	const bare = "virtio-net-pci,netdev=net0"
	for _, tc := range []struct {
		name    string
		mac     string
		want    string
		wantErr bool
	}{
		{"unset degrades to the QEMU default", "", bare, false},
		// " " used to produce a dangling "mac= " -- exactly the command line
		// the old doc comment claimed could not happen.
		{"whitespace degrades to the QEMU default", "   ", bare, false},
		{"tab and newline degrade too", "\t\n", bare, false},
		{"padded MAC passes through", "52:54:00:12:34:56", bare + ",mac=52:54:00:12:34:56", false},
		{"stripped MAC is padded", "52:54:0:12:34:56", bare + ",mac=52:54:00:12:34:56", false},
		{"uppercase MAC is lowercased", "52:54:00:AB:CD:EF", bare + ",mac=52:54:00:ab:cd:ef", false},
		{"surrounding whitespace is trimmed", " 52:54:00:12:34:56\n", bare + ",mac=52:54:00:12:34:56", false},
		// Comma is QEMU's option separator: concatenating this raw injects
		// further device properties instead of setting an address.
		{"option injection is rejected", "52:54:00:12:34:56,romfile=/tmp/evil.rom", "", true},
		{"trailing comma is rejected", "52:54:00:12:34:56,", "", true},
		{"garbage is rejected", "not-a-mac", "", true},
		{"five octets are rejected", "52:54:00:12:34", "", true},
		{"dash separated is rejected", "52-54-00-12-34-56", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := netDeviceArg(tc.mac)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("netDeviceArg(%q) = (%q, nil), want an error", tc.mac, got)
				}
				if got != "" {
					t.Errorf("netDeviceArg(%q) returned %q alongside its error", tc.mac, got)
				}
				if !strings.Contains(err.Error(), tc.mac) {
					t.Errorf("error %q should name the offending value %q", err, tc.mac)
				}
				return
			}
			if err != nil {
				t.Fatalf("netDeviceArg(%q): %v", tc.mac, err)
			}
			if got != tc.want {
				t.Errorf("netDeviceArg(%q) = %q, want %q", tc.mac, got, tc.want)
			}
		})
	}
}
