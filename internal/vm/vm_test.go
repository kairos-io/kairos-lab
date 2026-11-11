package vm

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestBuildLinuxCommandIncludesTapInBridgeMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, args, err := buildLinuxFor("amd64", StartConfig{
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
	_, _, err := buildLinuxFor("amd64", StartConfig{NetworkMode: "bridged"})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestBuildLinuxCommandSerialDisplayUsesNographic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, args, err := buildLinuxFor("amd64", StartConfig{
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
	_, args, err := buildLinuxFor("amd64", StartConfig{
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
}

// The arm64 window-display device block (virtio-gpu-pci, qemu-xhci, usb-kbd,
// usb-tablet) used to be guarded only by a runtime.GOARCH check in the test
// above, which keyed off the HOST arch rather than the goarch argument -- so
// it never ran on either CI leg (ubuntu-latest is amd64, macos-latest is
// darwin/arm64 but never calls buildLinuxFor). This is the real gate.
func TestBuildLinuxARM64WindowDisplayAddsTheGPUAndInputDevices(t *testing.T) {
	_, args, err := buildLinuxFor("arm64", StartConfig{
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "user",
		DisplayMode:   "window",
		BiosPath:      "/usr/share/AAVMF/QEMU_EFI.fd",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"virtio-gpu-pci", "qemu-xhci", "usb-kbd", "usb-tablet"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %s for arm64 window display mode: %s", want, joined)
		}
	}
	if strings.Contains(joined, "-nographic") {
		t.Errorf("did not expect -nographic for window display mode: %s", joined)
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
	const userNetdev = "user,id=net0,hostfwd=tcp:" + DefaultUserModeHostBind + ":2222-:22,hostfwd=tcp:" + DefaultUserModeHostBind + ":8080-:8080"
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
			_, args, err := buildLinuxFor("amd64", StartConfig{
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
	_, _, err := buildLinuxFor("amd64", StartConfig{NetworkMode: "shared"})
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
	_, args, err := buildLinuxFor("amd64", StartConfig{
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
	_, args, err := buildLinuxFor("amd64", StartConfig{
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
	_, args, err := buildLinuxFor("amd64", StartConfig{
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

// buildLinux itself -- the one line that binds the pure buildLinuxFor to the
// host's own runtime.GOARCH -- used to be asserted by nothing: every test in
// this file calls buildLinuxFor directly with an explicit arch, so a
// hardcoded binding (e.g. always "amd64") left the suite green while an
// arm64 host silently lost -machine and -bios again, exactly the 59c6949
// regression this file otherwise guards against.
//
// It carries no GOOS skip, and that is load-bearing rather than tidiness.
// On an amd64 host the "amd64" hardcode is an EQUIVALENT mutant -- it is
// literally the same call as buildLinuxFor(runtime.GOARCH, cfg) there, so no
// test can distinguish it. macos-latest is this repo's only arm64 CI leg, so
// a GOOS skip would leave the very mutation named above surviving every leg.
// Nothing stops it running there: buildLinux is pure, and cfg is chosen to
// succeed on either arch, since arm64 needs the non-empty BiosPath that
// amd64 simply ignores.
func TestBuildLinuxBindsToTheHostArchitecture(t *testing.T) {
	cfg := StartConfig{
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "user",
		BiosPath:      "/usr/share/AAVMF/QEMU_EFI.fd",
	}
	wantBinary, wantArgs, wantErr := buildLinuxFor(runtime.GOARCH, cfg)
	// Without this the test passes vacuously whenever BOTH sides fail: two
	// identical errors compare equal, so a future required-field guard on a
	// field this cfg leaves blank would silently disarm the comparison below.
	if wantErr != nil {
		t.Fatalf("buildLinuxFor(%q) must succeed for this comparison to mean anything: %v", runtime.GOARCH, wantErr)
	}
	gotBinary, gotArgs, gotErr := buildLinux(cfg)
	if gotBinary != wantBinary {
		t.Errorf("buildLinux binary = %q, want %q (buildLinuxFor(runtime.GOARCH, cfg))", gotBinary, wantBinary)
	}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Errorf("buildLinux args = %v, want %v (buildLinuxFor(runtime.GOARCH, cfg))", gotArgs, wantArgs)
	}
	// wantErr is nil past the Fatalf above, so this is the whole comparison:
	// buildLinux must not fail where buildLinuxFor(runtime.GOARCH) succeeded.
	if gotErr != nil {
		t.Errorf("buildLinux err = %v, want nil (buildLinuxFor(runtime.GOARCH, cfg) succeeded)", gotErr)
	}
}

// qemu-system-aarch64 has no default machine, so the arm64 command line the
// tool used to build died with "No machine specified" before it ever read the
// ISO (kairos-io/kairos#4858). No runtime.GOOS/GOARCH skip: buildLinuxFor is
// pure, so this runs on every host, including the amd64 CI leg.
func TestBuildLinuxARM64SuppliesMachineAcceleratorAndFirmware(t *testing.T) {
	const firmware = "/usr/share/AAVMF/QEMU_EFI.fd"
	binary, args, err := buildLinuxFor("arm64", StartConfig{
		ISOPath:     "/tmp/kairos.iso",
		DiskPath:    "/tmp/kairos.qcow2",
		CPUs:        2,
		MemoryMB:    4096,
		NetworkMode: "user",
		BiosPath:    firmware,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The arm64 binary selection used to be uncoupled from every existing
	// arm64 test, both of which discarded this return value: an arm64 host
	// would have launched qemu-system-x86_64 with no test noticing.
	if binary != "qemu-system-aarch64" {
		t.Errorf("arm64 binary = %q, want qemu-system-aarch64", binary)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-machine virt,gic-version=max",
		"-cpu host",
		"-enable-kvm",
		"-bios " + firmware,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("arm64 args are missing %q: %s", want, joined)
		}
	}
}

// A machine type belongs to arm64 only: adding one on amd64 would override
// QEMU's default q35/pc selection.
func TestBuildLinuxAMD64KeepsTheDefaultMachine(t *testing.T) {
	binary, args, err := buildLinuxFor("amd64", StartConfig{
		DiskPath:    "/tmp/kairos.qcow2",
		CPUs:        2,
		MemoryMB:    4096,
		NetworkMode: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if binary != "qemu-system-x86_64" {
		t.Errorf("got binary %q, want qemu-system-x86_64", binary)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-machine") || strings.Contains(joined, "-bios") {
		t.Errorf("amd64 should not carry a machine or firmware: %s", joined)
	}
	if !strings.Contains(joined, "-enable-kvm -cpu host") {
		t.Errorf("amd64 lost its accelerator: %s", joined)
	}
}

// Without firmware the guest boots to a blank screen, so refuse to build the
// command at all -- the same refusal buildMacOS already makes.
func TestBuildLinuxARM64RejectsAnEmptyFirmwarePath(t *testing.T) {
	// ISOPath, QGASocketPath and MACAddress are all set here so that only
	// BiosPath is left at its zero value. Leaving every optional field blank
	// let this test pass even when the production guard checked cfg.ISOPath
	// instead of cfg.BiosPath -- same error text, wrong field -- because
	// ISOPath being empty too tripped that wrong guard just as well.
	binary, args, err := buildLinuxFor("arm64", StartConfig{
		ISOPath:       "/tmp/kairos.iso",
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "user",
		MACAddress:    testMACAddress,
	})
	if err == nil {
		t.Fatalf("expected an error, got %s %v", binary, args)
	}
	if !strings.Contains(err.Error(), "firmware") {
		t.Errorf("error %q should name the missing firmware", err)
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

// The virt machine has no IDE controller, so the ide-cd the x86 path uses
// makes QEMU exit with "No 'IDE' bus found for device 'ide-cd'" before the
// firmware runs. See kairos-io/kairos#4858.
func TestBuildLinuxARM64AttachesTheISOToABusVirtHas(t *testing.T) {
	_, args, err := buildLinuxFor("arm64", StartConfig{
		ISOPath:     "/tmp/kairos.iso",
		DiskPath:    "/tmp/kairos.qcow2",
		CPUs:        2,
		MemoryMB:    4096,
		NetworkMode: "user",
		BiosPath:    "/usr/share/AAVMF/QEMU_EFI.fd",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "ide-cd") {
		t.Errorf("virt has no IDE bus, so the ISO cannot be an ide-cd: %s", joined)
	}
	for _, want := range []string{
		"-device virtio-scsi-pci",
		"-device scsi-cd,drive=cdrom1,bootindex=1",
		"-drive id=cdrom1,if=none,media=cdrom,file=/tmp/kairos.iso",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("arm64 ISO attachment is missing %q: %s", want, joined)
		}
	}
}

// The SCSI controller exists only to carry the CD, so a run without an ISO
// must not add it.
func TestBuildLinuxARM64AddsNoSCSIControllerWithoutAnISO(t *testing.T) {
	_, args, err := buildLinuxFor("arm64", StartConfig{
		DiskPath:    "/tmp/kairos.qcow2",
		CPUs:        2,
		MemoryMB:    4096,
		NetworkMode: "user",
		BiosPath:    "/usr/share/AAVMF/QEMU_EFI.fd",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "scsi") {
		t.Errorf("no ISO means no SCSI controller: %s", joined)
	}
}

// q35 has an IDE bus and no SCSI controller, so the amd64 attachment must not
// follow the arm64 one.
func TestBuildLinuxAMD64KeepsTheIDECD(t *testing.T) {
	_, args, err := buildLinuxFor("amd64", StartConfig{
		ISOPath:     "/tmp/kairos.iso",
		DiskPath:    "/tmp/kairos.qcow2",
		CPUs:        2,
		MemoryMB:    4096,
		NetworkMode: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-device ide-cd,drive=cdrom1,bootindex=1") {
		t.Errorf("amd64 lost its CD attachment: %s", joined)
	}
	if strings.Contains(joined, "scsi") {
		t.Errorf("amd64 should not gain a SCSI controller: %s", joined)
	}
}

// TestUserModeForwardsNeverBindEveryInterface is the regression test for
// kairos-io/kairos#5053.
//
// An empty host address in a QEMU hostfwd rule is INADDR_ANY, so
// "hostfwd=tcp::2222-:22" published a live Kairos guest's sshd -- and an
// installer ISO answers it with a documented default login -- on every
// interface of the host. This builds the netdev the way the affected callers
// do and asserts the shape that is never safe, rather than a single expected
// string, so a future caller that reintroduces an empty bind by some other
// route fails here too.
//
// It is GOOS-free on purpose: userModeNetdevArg is what both builders call,
// and the bug was in neither of them.
func TestUserModeForwardsNeverBindEveryInterface(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  StartConfig
	}{
		// The index-0 config, which is where the bug was: no ports and no
		// bind set, because index 0 took every default it could.
		{"nothing set at all", StartConfig{}},
		// Ports set, bind left unset: the shape a caller gets when it knows
		// about the port fields and not the bind one.
		{"ports set, bind unset", StartConfig{SSHPort: 2222, HTTPPort: 8080}},
		// Whitespace is not an address. netDeviceArg in this same file
		// already had to learn that " " slips past a bare == "" check.
		{"whitespace bind", StartConfig{UserModeHostBind: " "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netdev := userModeNetdevArg(tc.cfg)
			// "tcp::" is a hostfwd rule with no host address, which is the
			// bug verbatim.
			if strings.Contains(netdev, "tcp::") {
				t.Errorf("forwards bind every host interface (INADDR_ANY): %q", netdev)
			}
			for _, want := range []string{
				"hostfwd=tcp:" + DefaultUserModeHostBind + ":2222-:22",
				"hostfwd=tcp:" + DefaultUserModeHostBind + ":8080-:8080",
			} {
				if !strings.Contains(netdev, want) {
					t.Errorf("missing %q in %q", want, netdev)
				}
			}
		})
	}
}

// A caller that does name an address still gets it. Without this, deleting
// the field and hardcoding the default would keep the test above green.
func TestUserModeForwardsHonourAnExplicitBind(t *testing.T) {
	netdev := userModeNetdevArg(StartConfig{SSHPort: 2223, HTTPPort: 8081, UserModeHostBind: "192.0.2.7"})
	const want = "user,id=net0,hostfwd=tcp:192.0.2.7:2223-:22,hostfwd=tcp:192.0.2.7:8081-:8080"
	if netdev != want {
		t.Errorf("netdev = %q, want %q", netdev, want)
	}
}
