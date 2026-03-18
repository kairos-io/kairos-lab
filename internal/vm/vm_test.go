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
