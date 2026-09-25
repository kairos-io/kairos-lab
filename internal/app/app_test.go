package app

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

// Every kairos-lab option is a flag, so a positional argument is always a
// mistake. It used to be a silent one: `start /path/to.iso` left the path in
// the flag set's remaining args, -iso stayed empty, and the ISO was
// auto-selected from the download cache instead, so a VM booted from an image
// the user never named. See kairos-io/kairos#4432.
func TestRunRejectsPositionalArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"start iso path", []string{"start", "/tmp/kairos.iso"}, `unexpected argument "/tmp/kairos.iso": pass the ISO with -iso`},
		{"start flag then path", []string{"start", "-no-iso", "/tmp/kairos.iso"}, `unexpected argument "/tmp/kairos.iso": pass the ISO with -iso`},
		{"setup", []string{"setup", "qemu"}, `unexpected argument "qemu": setup takes flags only`},
		{"reset disk name", []string{"reset", "mydisk"}, `unexpected argument "mydisk": remove a single disk with -disk`},
		{"cleanup", []string{"cleanup", "all"}, `unexpected argument "all": cleanup takes flags only`},
		{"status", []string{"status", "vm"}, `unexpected argument "vm": status takes no arguments`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

			var stdout, stderr bytes.Buffer
			err := Run(tc.args, strings.NewReader(""), &stdout, &stderr, "test")
			if err == nil {
				t.Fatalf("%v was accepted, want an error", tc.args)
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// The guard must reject only the leftover argument. Without this the check
// could fail every invocation and still pass the table above.
func TestRunAcceptsSubcommandsWithoutPositionalArguments(t *testing.T) {
	for _, args := range [][]string{
		{"start", "-iso", "/tmp/kairos.iso"},
		{"reset", "-disk", "mydisk"},
		{"cleanup", "-dry-run"},
	} {
		t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
		t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

		var stdout, stderr bytes.Buffer
		err := Run(args, strings.NewReader(""), &stdout, &stderr, "test")
		// These all stop at requireSetup on a fresh config dir, which is
		// exactly the point: they got past the positional-argument guard.
		if !errors.Is(err, errSetupRequired) {
			t.Fatalf("%v: got %v, want %v", args, err, errSetupRequired)
		}
	}
}

// The config review can turn bridged mode on (option 7) without the interface
// (option 8) ever being touched, and runStart used to resolve the interface
// only before the review. The VM then reached ValidateBridgeIface with an
// empty name, which reported a link problem for an interface it never named.
// See kairos-io/kairos#4649.
func TestResolveBridgeIface(t *testing.T) {
	// User mode has no interface to resolve, and an interface the user chose
	// is never second-guessed. Neither case may probe the host.
	for _, tc := range []struct{ mode, iface string }{
		{"user", ""},
		{"user", "en1"},
		{"bridged", "en1"},
	} {
		got, err := resolveBridgeIface(tc.mode, tc.iface)
		if err != nil {
			t.Fatalf("resolveBridgeIface(%q, %q) errored: %v", tc.mode, tc.iface, err)
		}
		if got != tc.iface {
			t.Errorf("resolveBridgeIface(%q, %q) = %q, want it unchanged", tc.mode, tc.iface, got)
		}
	}

	// Bridged with no interface must come back with one or say why not. The
	// answer depends on the host's own links, but "" with no error is the
	// combination that produced the empty-name message, and it is never
	// right.
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	iface, err := resolveBridgeIface("bridged", "")
	if err == nil && iface == "" {
		t.Fatal("bridged mode resolved to no interface and no error")
	}
}

// reviewInput feeds the config review one line at a time. reviewVMConfig
// builds a fresh bufio.Reader on every pass of its loop, and a plain
// strings.Reader is drained into the first one's buffer, so the second pass
// sees EOF and the review returns "cancelled". Reading a byte at a time is
// what a terminal does anyway.
func reviewInput(lines ...string) io.Reader {
	return iotest.OneByteReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

// Switching option 7 to bridged has to leave the review with an interface in
// hand. TestResolveBridgeIface covers the helper, but nothing asserted that
// the review calls it, and the bug was the wiring: the menu redrew
// "8) Net interface:" with nothing after it and the user confirmed a
// configuration whose interface was still empty (kairos-io/kairos#4649).
func TestReviewResolvesIfaceWhenModeSwitchesToBridged(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("no bridge interface to resolve on %s", runtime.GOOS)
	}

	cfg := &vmStartConfig{
		DiskName:    "test",
		DiskPath:    filepath.Join(t.TempDir(), "test.qcow2"),
		DiskSize:    "20G",
		MemoryGB:    4,
		CPUs:        2,
		NetworkMode: "user",
		Display:     "serial",
		IsNewDisk:   true,
	}

	var stdout bytes.Buffer
	// Option 7, switch to bridged, then Enter to accept.
	got, err := reviewVMConfig(cfg, reviewInput("7", "bridged", ""), &stdout)
	if err != nil {
		t.Fatalf("review errored: %v", err)
	}
	if got.NetworkMode != "bridged" {
		t.Fatalf("network mode is %q, want bridged", got.NetworkMode)
	}

	// Either an interface came out, or the review said why one could not.
	// Silently empty is the combination the VM cannot start from.
	if got.NetworkIface == "" && !strings.Contains(stdout.String(), "bridged networking") {
		t.Fatalf("bridged mode left the interface empty and said nothing about it; review output:\n%s", stdout.String())
	}

	// The menu redraws after the switch, and that redraw is what the user
	// confirms. It may not show an empty interface.
	if got.NetworkIface != "" && strings.Contains(stdout.String(), "8) Net interface: \n") {
		t.Errorf("the review rendered a blank interface before asking for confirmation; output:\n%s", stdout.String())
	}
}
