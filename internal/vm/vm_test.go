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

func TestBuildLinuxCommandIncludesTapInVirbrMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, args, err := buildLinux(StartConfig{
		ISOPath:       "/tmp/kairos.iso",
		DiskPath:      "/tmp/kairos.qcow2",
		QGASocketPath: "/tmp/kairos.sock",
		CPUs:          2,
		MemoryMB:      4096,
		NetworkMode:   "virbr",
		LinuxTapName:  DefaultVirbrTapName,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ifname="+DefaultVirbrTapName) {
		t.Fatalf("expected virtap in args: %s", joined)
	}
}

func TestBuildLinuxCommandFailsWithoutTapInVirbrMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only test")
	}
	_, _, err := buildLinux(StartConfig{NetworkMode: "virbr"})
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
	if !strings.Contains(joined, "net="+userSlirpNetCIDR) || strings.Contains(joined, "10.0.2") {
		t.Fatalf("expected RFC1918 user slirp net in args: %s", joined)
	}
}

// qemu-system-aarch64 has no default machine, so the arm64 command line the
// tool used to build died with "No machine specified, and there is no
// default" before it ever read the ISO. See kairos-io/kairos#4858. The
// architecture is a parameter so this runs on an amd64 host too.
func TestBuildLinuxARM64SuppliesMachineAcceleratorAndFirmware(t *testing.T) {
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
	for _, want := range []string{
		"-machine virt,gic-version=max",
		"-cpu host",
		"-enable-kvm",
		"-bios /usr/share/AAVMF/QEMU_EFI.fd",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("arm64 args are missing %q: %s", want, joined)
		}
	}
}

// A machine type belongs to arm64 only: adding it on amd64 would override the
// default q35/pc selection.
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
// command at all, the way the macOS path already does.
func TestBuildLinuxARM64RejectsAnEmptyFirmwarePath(t *testing.T) {
	binary, args, err := buildLinuxFor("arm64", StartConfig{
		DiskPath:    "/tmp/kairos.qcow2",
		CPUs:        2,
		MemoryMB:    4096,
		NetworkMode: "user",
	})
	if err == nil {
		t.Fatalf("expected an error, got %s %v", binary, args)
	}
	if !strings.Contains(err.Error(), "firmware") {
		t.Errorf("error %q should name the missing firmware", err)
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
