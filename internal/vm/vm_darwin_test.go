//go:build darwin

package vm

import (
	"runtime"
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
