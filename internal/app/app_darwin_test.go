//go:build darwin

// Tests for the macOS half of a start. The build tag is what keeps them off
// the ubuntu leg, where the branch under test is unreachable: runStart
// launches QEMU under sudo only when vmnetNeedsSudo says so, and that is
// darwin and nothing else.
package app

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The vmnet sudo branch, driven end to end rather than through its two
// helpers.
//
// vmnetNeedsSudo and vmnetSudoPrompt are already pinned on either leg because
// they take goos as a parameter, and that was the answer to "this branch
// cannot be reached from a test". It was the wrong answer: what those two
// pin is a decision and a string, and neither of them shows that runStart
// acts on the decision -- that cmdName becomes "sudo", that the QEMU binary
// moves into the argument list behind it, and that this is what gets recorded
// as st.VM.QemuBinary and printed as the command about to run. Deleting the
// `cmdName = "sudo"` assignment leaves both of those tests green.
//
// Reaching it needs no seam and no production change. Between Run and the
// launch, the only thing this branch asks the host for is
// macOSFirmwarePath's `brew --prefix qemu` and an os.Stat of the firmware
// underneath the answer -- and `brew` is resolved through PATH, which the
// tests here already control. A two-line fake brew in a temp directory is the
// whole of the fixture. The launch itself then fails to find `sudo` on that
// same PATH, so the assertions below run over a process that was never
// started, on a host nothing was escalated on.
func TestStartLaunchesVmnetSharedUnderSudo(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skipf("macOS support is Apple Silicon only; on %s buildMacOS refuses before the launch is reached", runtime.GOARCH)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	firmware := fakeBrewOnPath(t)
	seedStartableState(t, "kairos-disk0")
	// The privilege pre-flight is vm.RequireNetworkPrivilege's decision and
	// is tested where it lives; here it is made to say yes so the run reaches
	// the launch.
	stubNetworkPrivilege(t, func(string) error { return nil })

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "shared", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test")

	// The launch is where this run stops, and it stops for the reason the
	// isolation arranges: there is no sudo on PATH. Anything else means the
	// run died earlier and the assertions below would be about a state
	// nothing wrote.
	if err == nil || !strings.Contains(err.Error(), "start qemu") {
		t.Fatalf("start returned %v, want it to have reached the QEMU launch; stdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("the launch failed on something other than sudo itself: %v", err)
	}

	st := loadStoredState(t)
	if st.VM.QemuBinary != "sudo" {
		t.Errorf("QemuBinary = %q, want %q: both vmnet modes need QEMU itself launched as root", st.VM.QemuBinary, "sudo")
	}
	if len(st.VM.QemuArgs) == 0 || st.VM.QemuArgs[0] != "qemu-system-aarch64" {
		t.Fatalf("QemuArgs = %q, want the QEMU binary as the first argument to sudo", st.VM.QemuArgs)
	}
	// The recorded command is the command: st.VM.QemuArgs is what `status`
	// and a later `stop` read, so the binary moving behind sudo has to be
	// visible there and not only in the exec.
	if !strings.Contains(stdout.String(), "Running: sudo qemu-system-aarch64") {
		t.Errorf("the printed command does not show the launch running under sudo:\n%s", stdout.String())
	}
	// And it is a vmnet-shared launch, over no host interface at all: the
	// firmware path is the one the fake brew answered with, which is what
	// says macOSFirmwarePath really ran on the way here.
	if valueAfterArg(st.VM.QemuArgs, "-netdev") != "vmnet-shared,id=net0" {
		t.Errorf("QemuArgs = %q, want a bare vmnet-shared netdev", st.VM.QemuArgs)
	}
	if got := valueAfterArg(st.VM.QemuArgs, "-bios"); got != firmware {
		t.Errorf("-bios = %q, want %q -- the path the fake brew answered with", got, firmware)
	}
	if st.Network.BridgeInterface != "" {
		t.Errorf("BridgeInterface = %q, want it empty: shared attaches to no host interface", st.Network.BridgeInterface)
	}
}

// fakeBrewOnPath is isolateFromHostBinaries with exactly one exception: PATH
// holds a `brew` that answers `--prefix qemu` with a directory this test
// owns, and the firmware file macOSFirmwarePath stats is written underneath
// it. It returns that firmware path.
//
// Everything else stays unreachable, which is the point of pointing PATH at a
// directory of our own rather than adding to the host's: `sudo` is not found,
// so the launch never escalates anything, and neither is qemu-img or
// qemu-system-aarch64, so no image is written and no VM starts.
func fakeBrewOnPath(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	prefix := t.TempDir()
	firmware := filepath.Join(prefix, "share", "qemu", "edk2-aarch64-code.fd")
	if err := os.MkdirAll(filepath.Dir(firmware), 0o755); err != nil {
		t.Fatalf("create firmware directory: %v", err)
	}
	if err := os.WriteFile(firmware, []byte("not firmware, never read\n"), 0o644); err != nil {
		t.Fatalf("write firmware: %v", err)
	}
	// Single-quoted so a temp directory with a space or a '#' in it survives
	// the shell; t.TempDir never produces one with a quote in it.
	script := "#!/bin/sh\necho '" + prefix + "'\n"
	if err := os.WriteFile(filepath.Join(binDir, "brew"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake brew: %v", err)
	}
	t.Setenv("PATH", binDir)
	return firmware
}

// valueAfterArg returns the value following a flag in a QEMU argument list,
// or "" when the flag is absent or is the last word there.
func valueAfterArg(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}
