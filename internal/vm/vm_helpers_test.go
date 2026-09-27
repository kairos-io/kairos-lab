// Helpers shared by the QEMU command-line tests of BOTH builders: vm_test.go
// (buildLinux) and vm_darwin_test.go (buildMacOS) call them.
//
// This file MUST stay untagged. vm_test.go guards each of its tests with a
// runtime GOOS check rather than a build tag, which invites "tidying" the pair
// into //go:build linux -- and the moment a constraint lands on this file the
// macOS leg fails to compile with "undefined: testMACAddress" in a file whose
// author never opened it. Nothing on a linux host notices: only
// `GOOS=darwin GOARCH=arm64 go vet ./internal/vm/` compiles vm_darwin_test.go,
// since `go build` does not read _test.go files at all.
package vm

import (
	"strings"
	"testing"
)

// testMACAddress is a fixed literal rather than a MACForDisk call: these tests
// pin how a MAC reaches the command line, not how one is derived.
const testMACAddress = "52:54:00:aa:bb:cc"

// argAfter returns the single value following flag in args.
func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Fatalf("%s is the last argument, it has no value: %v", flag, args)
			}
			return args[i+1]
		}
	}
	t.Fatalf("%s missing from args: %v", flag, args)
	return ""
}

// nicDeviceArg returns the -device value that configures the guest NIC. There
// are several -device arguments, so it matches on the device model.
func nicDeviceArg(t *testing.T, args []string) string {
	t.Helper()
	for _, a := range args {
		if strings.HasPrefix(a, "virtio-net-pci") {
			return a
		}
	}
	t.Fatalf("no virtio-net-pci device in args: %v", args)
	return ""
}

// deviceArgs returns every value that follows a -device flag, so a test can
// assert a device by exact spelling instead of a substring of the joined
// command line ("virtio-serial" is a prefix of "virtio-serial-pci").
func deviceArgs(args []string) []string {
	var devices []string
	for i, a := range args {
		if a == "-device" && i+1 < len(args) {
			devices = append(devices, args[i+1])
		}
	}
	return devices
}

// assertOneNIC pins that the guest gets exactly one network backend and
// exactly one NIC. argAfter and nicDeviceArg both return the FIRST match, so
// without this a second -netdev or a second virtio-net-pci device appended
// anywhere after the first is invisible to every other assertion -- while
// QEMU would either fail to start on the duplicate id or hand the guest a
// second, unconfigured interface.
func assertOneNIC(t *testing.T, args []string) {
	t.Helper()
	netdevs := 0
	for _, a := range args {
		if a == "-netdev" {
			netdevs++
		}
	}
	if netdevs != 1 {
		t.Errorf("want exactly 1 -netdev flag, got %d: %v", netdevs, args)
	}
	nics := 0
	for _, d := range deviceArgs(args) {
		if strings.HasPrefix(d, "virtio-net-pci") {
			nics++
		}
	}
	if nics != 1 {
		t.Errorf("want exactly 1 virtio-net-pci device, got %d: %v", nics, args)
	}
}
