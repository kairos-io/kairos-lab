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
