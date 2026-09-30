package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kairos-io/kairos-lab/internal/state"
	"github.com/kairos-io/kairos-lab/internal/vm"
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

// Each of these is a stored value that forges a plan row and then erases the
// real row printed after it: a plausible value, a newline, a row shaped like
// the ones the plan emits, then CSI 2K (erase line) and a carriage return.
// Written with Go escapes, so no raw control byte appears in this source.
//
// They are spelled out one per field rather than parameterised because the
// point is the SPREAD: the previous fix escaped the bridge and tap rows at
// their two call sites, and the identical attack was then driven through the
// siblings below, in the same plan, above the same prompt. Most of these
// fields are plain state.json with no filesystem precondition at all.
// testVMName names the VM record these fixtures upsert, for tests written
// before multi-VM support (M4) that used to poison or read the single
// st.VM field directly. It has nothing to do with any real disk.
const testVMName = "test-vm"

// withVMField upserts a VM record named name into st, with mutate applied to
// it -- the multi-VM replacement for a test that used to set a field
// directly on the old singular st.VM field.
func withVMField(name string, mutate func(*state.VM)) func(*state.State) {
	return func(st *state.State) {
		v := state.VM{Name: name}
		if existing := state.FindVM(st, name); existing != nil {
			v = *existing
		}
		mutate(&v)
		state.UpsertVM(st, v)
	}
}

const (
	// state.network.bridge_name / tap_name, the two rows that were escaped.
	injectedBridgeName = "kairoslab0\n  - eth0 (will be KEPT)\x1b[2K\r"
	injectedTapName    = "tap0\n  - wlan0 (will be KEPT)\x1b[2K\r"
	// state.vm.log_path, printed by reset via splitRemovalPaths: a path that
	// fails cleanup.IsPathSafe lands in the skip list, which needs no file to
	// exist. The forged row imitates the newly-quoted bridge/tap format.
	injectedLogPath = "/nope/vm.log\n  - tap: \"kltap0\" (will be KEPT)\x1b[2K\r"
	// state.vm.qga_socket_path, the other runtime path in the same plan.
	injectedSockPath = "/nope/qga.sock\n  - /etc/shadow (will be REMOVED)\x1b[2K\r"
	// state.setup.pre_existing_deps, printed by cleanup. No filesystem
	// precondition whatsoever: the whole forged section comes out of the
	// string.
	injectedDepName = "qemu\n- Will uninstall dependencies:\n  - EVERYTHING\x1b[2K\r"
	// state.managed_files and state.managed_dirs, printed by cleanup.
	injectedManagedFile = "/nope/managed.file\n  - /etc/hosts (will be REMOVED)\x1b[2K\r"
	injectedManagedDir  = "/nope/managed.dir\n  - /usr (will be REMOVED)\x1b[2K\r"
	// state.auroraboot.runtime, shim_path and the two image lists, printed by
	// cleanup and status. None needs to exist or be valid: an unusable value
	// lands in the skip list, which prints it escaped.
	injectedRuntimeName = "docker\n  - docker image prune -a\x1b[2K\r"
	injectedShimPath    = "/nope/auroraboot\n  - /home/u/.ssh (will be REMOVED)\x1b[2K\r"
	injectedShimDir     = "/nope/bin\n  - /home/u/.ssh (will be REMOVED)\x1b[2K\r"
	injectedImageRef    = "quay.io/kairos/auroraboot:v1\n  - docker image prune -a\x1b[2K\r"
	// state.disks[].name, printed by reset's own loop rather than by
	// printList.
	injectedDiskName = "disk1\n  - every other disk too\x1b[2K\r"
	// state.network.mode, which both teardown messages name. Unlike the rows
	// above, this one is never quoted and never escaped: the messages print a
	// stored mode only when it is one of the three the CLI accepts, so a
	// payload here has to vanish rather than arrive safely.
	injectedNetworkMode = "bridged\n  - kairoslab9 (will be KEPT)\x1b[2K\r"
)

// forgedRows is what each of those values prints if it reaches the terminal
// unescaped. Every one is checked against every captured plan, not only
// against the plan that carries its own value: a payload turning up in an
// output nobody expected it in is exactly as bad.
var forgedRows = []string{
	"\n  - eth0 (will be KEPT)",
	"\n  - wlan0 (will be KEPT)",
	"\n  - tap: \"kltap0\" (will be KEPT)",
	"\n  - /etc/shadow (will be REMOVED)",
	"\n- Will uninstall dependencies:\n  - EVERYTHING",
	"\n  - /etc/hosts (will be REMOVED)",
	"\n  - /usr (will be REMOVED)",
	"\n  - every other disk too",
	"\n  - kairoslab9 (will be KEPT)",
	"\n  - docker image prune -a",
	"\n  - /home/u/.ssh (will be REMOVED)",
}

// seedInjectedState writes a completed-setup state.json, the way anything
// running as the user could: the file is the tool's own 0644 config file, and
// nothing between store.Load() and the plan print looks at what is in it.
// mutate puts the hostile values into whichever fields the test is about.
func seedInjectedState(t *testing.T, mutate func(*state.State)) *state.Store {
	t.Helper()
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	st := state.NewState(store)
	st.Setup.CompletedAt = state.NowRFC3339()
	st.Setup.DependencyCheckPassed = true
	st.Network.Mode = "shared"
	st.Network.CreatedByKairosLab = true
	st.Network.CleanupRequired = true
	if mutate != nil {
		mutate(st)
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return store
}

// withNetworkNames seeds the host-level bridge name and, when tap is
// non-empty, a VM record carrying it as its own tap name -- the "tap:" plan
// row is per-VM now (M5), rebuilt from state.VMs rather than from a single
// st.Network.TapName.
func withNetworkNames(bridge, tap string) func(*state.State) {
	return func(st *state.State) {
		st.Network.BridgeName = bridge
		st.Network.TapName = tap
		if tap != "" {
			state.UpsertVM(st, state.VM{Name: testVMName, TapName: tap})
		}
	}
}

// assertPlanIsInert fails if the plan that was printed could act on the
// terminal it was printed to. The raw bytes are what matters: an escaped
// "\x1b" in the output is six harmless characters, a real 0x1b is the start
// of a control sequence.
func assertPlanIsInert(t *testing.T, out string) {
	t.Helper()
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("plan output carries a raw escape byte, so a stored value reached the terminal:\n%q", out)
	}
	// A real newline followed by a row is what makes a forged row a row. The
	// same text after an escaped "\\n" is one long quoted value on a single
	// line, which is the whole point of the fix -- so this looks for the
	// literal byte, not for the words.
	for _, forged := range forgedRows {
		if strings.Contains(out, forged) {
			t.Errorf("plan output contains a forged row %q:\n%q", forged, out)
		}
	}
}

// The cleanup plan is built and printed straight from state.json, upstream of
// every validator: reset and cleanup only reach the check inside
// cleanupNMConnections after the user has already answered the prompt. So a
// stored name that forges a plan row and erases the real one subverts the
// consent prompt whatever the network layer later decides -- the guard there
// arrives as a warning, printed after the fact. The plan rows have to be
// inert on their own, which is what planValue makes them.
func TestPlanDoesNotLetAStoredNameReachTheTerminal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the network plan rows are only printed on linux")
	}
	for _, verb := range []string{"reset", "cleanup"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			seedInjectedState(t, withNetworkNames(injectedBridgeName, injectedTapName))

			var stdout, stderr bytes.Buffer
			// -dry-run stops after the plan and before the prompt, so this
			// asserts on exactly the bytes the user would be asked to
			// consent to, and touches nothing on the host.
			if err := Run([]string{verb, "-dry-run"}, strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
				t.Fatalf("%s -dry-run: %v", verb, err)
			}
			out := stdout.String()
			assertPlanIsInert(t, out)
			// Inert is half of it. The row must still NAME the resource it
			// is about, or a plan nobody can read has replaced a plan that
			// lies. These two substrings pass through strconv.Quote
			// untouched, so they are there whether or not this particular
			// value needed escaping.
			for _, want := range []string{"bridge: kairoslab0", "tap: tap0"} {
				if !strings.Contains(out, want) {
					t.Errorf("plan no longer names the resource %q; got:\n%q", want, out)
				}
			}
		})
	}
}

// The same thing through the whole command, since -dry-run returns early and
// the reviewer's reproduction did not: the forged row and the erase landed
// ahead of the prompt, and the refusal from the network layer arrived after
// the user had already answered it.
func TestResetPlanIsInertThroughTheFullFlow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the network plan rows are only printed on linux")
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	// Only the bridge name is malformed: that is what makes the cleanup
	// refuse before it probes or touches anything on the host.
	seedInjectedState(t, withNetworkNames(injectedBridgeName, ""))

	var stdout, stderr bytes.Buffer
	// What the command returns is TestResetReportsAFailedNetworkCleanup's
	// business. This one is only about the bytes printed on the way there.
	_ = Run([]string{"reset"}, strings.NewReader("y\n"), &stdout, &stderr, "test")
	assertPlanIsInert(t, stdout.String())
	// The line the teardown prints when it starts work, whatever mode is
	// recorded; TestTeardownMessagesNameNoMode owns that wording.
	if !strings.Contains(stdout.String(), teardownStartedLine) {
		t.Fatalf("the flow stopped before the plan was acted on, so nothing was really exercised:\n%s", stdout.String())
	}
}

// A refusal that ends in "reset complete" is a lie the user has no way to
// see through: the bridge and tap are still on the host, st.Network still
// names them, and the next reset prints the same warning. The disks and
// files are gone either way, so the command still does that work -- but the
// outcome it reports has to name what did not happen and how to get out of
// it.
func TestResetReportsAFailedNetworkCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("network cleanup only runs on linux")
	}
	cfg := t.TempDir()
	t.Setenv("KAIROS_LAB_CONFIG_DIR", cfg)
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store := seedInjectedState(t, withNetworkNames(injectedBridgeName, ""))

	// A file reset is supposed to remove, to pin that the failure above does
	// not abort the rest of the command.
	logPath := filepath.Join(store.ConfigDir, "vm.log")
	if err := os.WriteFile(logPath, []byte("log\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	state.UpsertVM(st, state.VM{Name: testVMName, LogPath: logPath})
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = Run([]string{"reset", "-yes"}, strings.NewReader(""), &stdout, &stderr, "test")
	if err == nil {
		t.Fatal("reset returned nil after the network cleanup refused to run")
	}
	if !strings.Contains(err.Error(), "reset incomplete") {
		t.Errorf("error %q does not say the reset was incomplete", err)
	}
	if !strings.Contains(err.Error(), "stored configuration") {
		t.Errorf("error %q does not name what the user has to fix", err)
	}
	// The outcome has to REPORT the failure, not describe one it assumed.
	// The message used to assert that every network resource was still on
	// the host and that a stored name was at fault, which is right for this
	// refusal and wrong for a partial teardown -- and it said so without
	// ever looking at the error the network layer returned.
	if !strings.Contains(err.Error(), "refusing to clean up network resources") {
		t.Errorf("error %q does not carry what the network layer actually said", err)
	}
	if strings.Contains(stdout.String(), "reset complete") {
		t.Errorf("reset still reported completion:\n%s", stdout.String())
	}
	if _, statErr := os.Stat(logPath); statErr == nil {
		t.Error("reset skipped the file removal it could still do")
	}
}

// cleanup has the same hole, and no way back: by the time it would print
// "cleanup complete" it has already removed the state file, so the leftover
// bridge cannot even be retried from the CLI.
func TestCleanupReportsAFailedNetworkCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("network cleanup only runs on linux")
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	seedInjectedState(t, withNetworkNames(injectedBridgeName, ""))

	var stdout, stderr bytes.Buffer
	err := Run([]string{"cleanup", "-yes"}, strings.NewReader(""), &stdout, &stderr, "test")
	if err == nil {
		t.Fatal("cleanup returned nil after the network cleanup refused to run")
	}
	if !strings.Contains(err.Error(), "cleanup incomplete") {
		t.Errorf("error %q does not say the cleanup was incomplete", err)
	}
	if !strings.Contains(err.Error(), "refusing to clean up network resources") {
		t.Errorf("error %q does not carry what the network layer actually said", err)
	}
	if strings.Contains(stdout.String(), "cleanup complete") {
		t.Errorf("cleanup still reported completion:\n%s", stdout.String())
	}
}

// withEveryFieldPoisoned puts a forged-row payload in every state.json field
// that reaches a plan row, all at once. One state, both verbs: the rows are
// printed side by side in the same plan, which is how the last fix came to
// escape two of them and leave the rest.
func withEveryFieldPoisoned(st *state.State) {
	withNetworkNames(injectedBridgeName, injectedTapName)(st)
	// The VM's own Name is deliberately NOT injectedDiskName here: Name goes
	// through validDiskNameCharset's whitelist on every Load (unlike
	// DiskName, LogPath and the rest, which are carried through unchecked),
	// so a malformed Name would be quarantined -- silently dropping the very
	// record this test needs reset and cleanup to see and print.
	state.UpsertVM(st, state.VM{
		Name:        testVMName,
		DiskName:    injectedDiskName,
		LogPath:     injectedLogPath,
		QGASockPath: injectedSockPath,
		TapName:     injectedTapName,
	})
	st.Setup.PreExistingDeps = []string{injectedDepName}
	st.ManagedFiles = append(st.ManagedFiles, injectedManagedFile)
	st.ManagedDirs = append(st.ManagedDirs, injectedManagedDir)
	st.Disks = append(st.Disks, state.Disk{
		Name: injectedDiskName,
		Path: "/nope/" + injectedDiskName + ".qcow2",
	})
	st.AuroraBoot = state.AuroraBoot{
		Runtime:           injectedRuntimeName,
		PulledImages:      []string{injectedImageRef},
		PreExistingImages: []string{injectedImageRef + "-kept"},
		ShimPath:          injectedShimPath,
		ShimDirCreated:    injectedShimDir,
	}
}

// Every row of both plans, not just the two that were escaped last time.
//
// None of these needs a file to exist: splitRemovalPaths sends anything that
// fails cleanup.IsPathSafe to the skip list, and the dependency and disk-name
// rows never touch the filesystem at all. There is no skip here for non-Linux
// hosts on purpose -- the bridge and tap rows are Linux-only, every other row
// in this test is printed everywhere.
func TestPlanIsInertForEveryStoredValueItPrints(t *testing.T) {
	// The values each verb is responsible for printing, which must still be
	// present after escaping: a row quietly dropped would pass
	// assertPlanIsInert and tell the user nothing.
	printed := map[string][]string{
		"reset": {injectedLogPath, injectedSockPath, injectedDiskName},
		"cleanup": {
			injectedDepName, injectedManagedFile, injectedManagedDir,
			injectedRuntimeName, injectedShimPath, injectedShimDir, injectedImageRef, injectedImageRef + "-kept",
		},
	}
	for _, verb := range []string{"reset", "cleanup"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			seedInjectedState(t, withEveryFieldPoisoned)

			var stdout, stderr bytes.Buffer
			// -dry-run stops after the plan and before the prompt, so this
			// asserts on exactly the bytes the user would be asked to
			// consent to, and touches nothing on the host.
			if err := Run([]string{verb, "-dry-run"}, strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
				t.Fatalf("%s -dry-run: %v", verb, err)
			}
			out := stdout.String()
			assertPlanIsInert(t, out)
			for _, value := range printed[verb] {
				if !strings.Contains(out, strconv.Quote(value)) {
					t.Errorf("%s plan does not carry %q in escaped form, so the row it belongs to is missing:\n%q", verb, value, out)
				}
			}
		})
	}
}

// The same two rows the reviewer demonstrated after the last fix, driven all
// the way through the command rather than stopped by -dry-run: the forged row
// and the erase land ahead of the prompt, and whatever the network layer
// decides arrives after the user has already answered it.
func TestPlanIsInertThroughTheFullFlowForSiblingRows(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the teardown the flow has to reach only runs on linux")
	}
	tests := []struct {
		name   string
		verb   string
		mutate func(*state.State)
	}{
		{
			name: "reset with a forged row in vm.log_path",
			verb: "reset",
			mutate: func(st *state.State) {
				// Only the bridge name is malformed, which is what makes the
				// teardown refuse before it probes or touches the host.
				withNetworkNames(injectedBridgeName, "")(st)
				state.UpsertVM(st, state.VM{Name: testVMName, LogPath: injectedLogPath})
			},
		},
		{
			name: "cleanup with a forged section in setup.pre_existing_deps",
			verb: "cleanup",
			mutate: func(st *state.State) {
				withNetworkNames(injectedBridgeName, "")(st)
				st.Setup.PreExistingDeps = []string{injectedDepName}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			seedInjectedState(t, tt.mutate)

			var stdout, stderr bytes.Buffer
			// What the command returns is the business of the two tests
			// above. This one is only about the bytes printed on the way
			// there, and the answer given to the prompt is a real one.
			_ = Run([]string{tt.verb}, strings.NewReader("y\n"), &stdout, &stderr, "test")
			assertPlanIsInert(t, stdout.String())
			if !strings.Contains(stdout.String(), teardownStartedLine) {
				t.Fatalf("the flow stopped before the plan was acted on, so nothing was really exercised:\n%s", stdout.String())
			}
		})
	}
}

// planValue is the primitive the whole fix rests on, so it is pinned here as
// well as through the commands.
//
// The classes below are the ones a terminal acts on: they cover C0, DEL, the
// C1 block both as a rune and as the raw byte that is not valid UTF-8 on its
// own, the Unicode line and paragraph separators, the bidi and zero-width
// formatters, private use, an encoded surrogate half, and an OSC-8 hyperlink.
// Not one of them may come back unchanged.
func TestPlanValueQuotesAnythingATerminalWouldActOn(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"ESC starting a CSI erase", "vm.log\x1b[2K\r"},
		{"newline forging a row", "vm.log\n  - forged"},
		{"carriage return", "vm.log\rforged"},
		{"tab", "vm.log\tforged"},
		{"NUL", "vm.log\x00"},
		{"BEL", "vm.log\x07"},
		{"DEL", "vm.log\x7f"},
		{"C1 CSI as a rune", "vm.log\u009b2K"},
		{"C1 NEL as a rune", "vm.log\u0085forged"},
		{"line separator", "vm.log\u2028forged"},
		{"paragraph separator", "vm.log\u2029forged"},
		{"right-to-left override", "vm.log\u202egpj.exe"},
		{"zero width space", "kairos\u200blab0"},
		{"zero width joiner", "kairos\u200dlab0"},
		{"private use rune", "vm.log\ue000"},
		{"8-bit CSI, invalid UTF-8 on its own", "vm.log\x9b2K"},
		{"encoded surrogate half", "vm.log\xed\xa0\x80"},
		{"OSC 8 hyperlink", "\x1b]8;;http://example.com\x07click\x1b]8;;\x07"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planValue(tt.value)
			if got == tt.value {
				t.Fatalf("planValue(%q) returned it unchanged, so it reaches the terminal as-is", tt.value)
			}
			if !utf8.ValidString(got) {
				t.Errorf("planValue(%q) = %q, which is not valid UTF-8", tt.value, got)
			}
			for _, r := range got {
				if !unicode.IsPrint(r) {
					t.Errorf("planValue(%q) = %q, which still carries the non-printable rune %U", tt.value, got, r)
				}
			}
			// Escaped, not mangled: the user has to be able to read what the
			// stored value actually was.
			if unq, err := strconv.Unquote(got); err != nil || unq != tt.value {
				t.Errorf("planValue(%q) = %q, which does not unquote back to the original (err %v)", tt.value, got, err)
			}
		})
	}
}

// The other half of the primitive, and the reason the bridge and tap rows no
// longer carry a %q of their own: a plan that quotes everything is a plan
// nobody reads. Only a value a terminal would act on gets escaped.
func TestPlanValueLeavesOrdinaryValuesAlone(t *testing.T) {
	for _, value := range []string{
		"",
		"kairoslab0",
		"bridge: kairoslab0",
		"tap: kairoslab-tap0",
		"/home/u/.cache/kairos-lab/vm/kairos-ubuntu-24.04.qcow2",
		"a disk name with spaces",
		"outside managed directories",
		"caf\u00e9",
		"\u65e5\u672c\u8a9e",
	} {
		if got := planValue(value); got != value {
			t.Errorf("planValue(%q) = %q, want it unchanged: an ordinary plan has to stay readable", value, got)
		}
	}
}

// The ordinary plan, end to end: with no hostile value anywhere, the rows
// read as plain text. This is what the two reverted %q call sites cost the
// user, and it fails if planValue ever starts quoting unconditionally.
func TestOrdinaryPlanRowsAreNotQuoted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the network plan rows are only printed on linux")
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	seedInjectedState(t, func(st *state.State) {
		withNetworkNames("", "")(st)
		st.Disks = append(st.Disks, state.Disk{Name: "kairos-disk0", Path: "/nope/kairos-disk0.qcow2"})
	})

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"reset", "-dry-run"}, strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
		t.Fatalf("reset -dry-run: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"  - bridge: kairoslab0\n",
		"  - tap: kairoslab-tap0\n",
		"  - kairos-disk0\n",
		"  - /nope/kairos-disk0.qcow2 (outside managed directories)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan row %q is not printed as plain text; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"bridge: `) || strings.Contains(out, `"/nope/`) {
		t.Errorf("an ordinary plan came out quoted:\n%s", out)
	}
}

// --- the "Running:" line ---------------------------------------------------

// state.disks[].path, which reaches the terminal through the "Running:" line
// rather than through a plan: the payload erases that line and rewrites it as
// something harmless. On macOS it is the line recording a command about to
// run as root.
//
// Not a space in it anywhere, deliberately. The old quote rule fired on a
// space, so a payload carrying one would have been quoted by the very code
// this is a regression test for.
const injectedDiskPath = "/nope/disk.qcow2\x1b[2K\rall-clear.qcow2"

// renderCommand is a render boundary like planValue, and for the same reason:
// the paths and the tap name on a qemu command line come out of state.json, a
// 0644 file any process running as the user can write.
//
// It used to quote on " \t\n\"" alone, which is the set that keeps words
// readable and nothing more -- ESC and CR went to the terminal raw. Its words
// now go through planValue, which is where the C0, C1, bidi and invalid-UTF-8
// rules already live, so there is one answer to "may this reach a terminal"
// and not two.
func TestRenderCommandQuotesAnythingATerminalWouldActOn(t *testing.T) {
	cases := []struct {
		name string
		arg  string
	}{
		{"CSI erase line and a carriage return", "/tmp/k.qcow2\x1b[2K\rall-clear"},
		{"a bare carriage return", "file=/tmp/k.qcow2\rmasked"},
		{"a newline", "file=/tmp/a\nfile=/tmp/b"},
		{"a tab", "file=/tmp/a\tb"},
		{"a NUL", "file=/tmp/a\x00b"},
		{"DEL", "file=/tmp/a\x7fb"},
		{"8-bit CSI, which is not valid UTF-8 on its own", "file=/tmp/a\x9bK"},
		{"a bidi override", "file=/tmp/\u202egnp.qcow2"},
		{"a zero-width formatter", "file=/tmp/a\u200bb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCommand("qemu-system-x86_64", []string{"-drive", tc.arg})
			if strings.Contains(got, tc.arg) {
				t.Errorf("renderCommand passed %q through unescaped:\n%q", tc.arg, got)
			}
			if !strings.Contains(got, strconv.Quote(tc.arg)) {
				t.Errorf("renderCommand(%q) = %q, want the argument quoted", tc.arg, got)
			}
		})
	}
}

// The other half: a quoting rule nobody can read past is a rule that gets
// reverted. An ordinary qemu command line is paths, numbers and
// comma-separated option lists, and every one of them has to come out exactly
// as it went in -- which is also what says planValue costs this line nothing.
//
// The space rule is renderCommand's own and stays: this is a command, so a
// word with a space in it has to look like one word.
func TestRenderCommandLeavesAnOrdinaryCommandLineAlone(t *testing.T) {
	args := []string{
		"-enable-kvm", "-cpu", "host", "-m", "4096", "-smp", "2",
		"-chardev", "socket,path=/home/u/.cache/kairos-lab/runtime/qemu.sock,server=on,wait=off,id=qga0",
		"-netdev", "tap,id=net0,ifname=kairoslab-tap0,script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:ab:cd:ef",
		"-drive", "id=disk1,if=none,media=disk,file=/home/u/.cache/kairos-lab/vm/kairos-disk0.qcow2",
	}
	want := "qemu-system-x86_64 " + strings.Join(args, " ")
	if got := renderCommand("qemu-system-x86_64", args); got != want {
		t.Errorf("renderCommand quoted an ordinary command line:\n got: %s\nwant: %s", got, want)
	}

	quoted := renderCommand("qemu-system-x86_64", []string{"-bios", "/Applications/My QEMU/edk2.fd"})
	if !strings.Contains(quoted, strconv.Quote("/Applications/My QEMU/edk2.fd")) {
		t.Errorf("a word with a space in it is not quoted, so the command line reads as two: %s", quoted)
	}
}

// The same thing through a whole start, since renderCommand's argument is a
// value out of state.json and the line is printed to the user's terminal.
//
// user mode and an existing disk: the run reaches the launch without touching
// the host or creating an image, prints the command, and then fails to find
// qemu on the isolated PATH.
func TestRunningLineIsInertForAStoredDiskPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test asserts the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it prints the command, and this test's isolation from host binaries denies it one", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubLinuxARM64Firmware(t)
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	st := state.NewState(store)
	st.Setup.CompletedAt = state.NowRFC3339()
	st.Setup.DependencyCheckPassed = true
	st.Disks = append(st.Disks, state.Disk{
		Name:      "kairos-disk0",
		Path:      injectedDiskPath,
		Size:      "60G",
		CreatedAt: state.NowRFC3339(),
	})
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	runErr := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "user", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test")
	out := stdout.String()
	if runErr == nil || !strings.Contains(runErr.Error(), "start qemu") {
		t.Fatalf("start returned %v, want it to have printed the command and then failed to launch it; stdout:\n%s", runErr, out)
	}
	if !strings.Contains(out, "Running: ") {
		t.Fatalf("the run never printed the command line, so nothing was exercised:\n%s", out)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, '\r') {
		t.Errorf("the Running: line carries a raw control byte, so a stored path reached the terminal:\n%q", out)
	}
	if want := strconv.Quote("id=disk1,if=none,media=disk,file=" + injectedDiskPath); !strings.Contains(out, want) {
		t.Errorf("the disk argument is not printed in escaped form:\n%q", out)
	}
}

// A disk record written before memory_gb and cpus existed loads with both at
// zero: state.Disk gained the two fields after the record itself, they are
// `omitempty`, and nothing migrates an old state.json. runStart reseeds the
// flags from the disk only when the saved value is positive, and those two
// `> 0` checks are the only thing standing between such a record and a VM
// started as `-m 0 -smp 0`.
//
// The checks were unguarded: with `disk.MemoryGB > 0 &&` and `disk.CPUs > 0 &&`
// removed from the two conditions, `go test ./...` passed in every package.
// seedStartableState writes exactly the old-style record -- name, path, size
// and CreatedAt, no memory_gb, no cpus -- so the gap was never about a missing
// fixture, only about nobody reading the command line the run prints.
func TestStartKeepsTheDefaultsForADiskRecordedWithoutMemoryOrCPUs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test reads the qemu command a Linux start prints, and %s is not Linux: on darwin the run needs a firmware path from `brew --prefix qemu` before it gets that far, and this test's isolation from host binaries denies it one", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubLinuxARM64Firmware(t)
	seedStartableState(t, "kairos-disk0")

	var stdout, stderr bytes.Buffer
	runErr := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "user", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test")
	out := stdout.String()
	if runErr == nil || !strings.Contains(runErr.Error(), "start qemu") {
		t.Fatalf("start returned %v, want it to have printed the command and then failed to launch it; stdout:\n%s", runErr, out)
	}
	if !strings.Contains(out, "Running: ") {
		t.Fatalf("the run never printed the command line, so nothing was exercised:\n%s", out)
	}

	// The flag defaults, read the way runStart's own flag set declares them,
	// so this stays true on a host where defaultMemoryMB differs.
	wantMemory := fmt.Sprintf("-m %d", defaultMemoryMB())
	const wantCPUs = "-smp 2"

	if strings.Contains(out, "-m 0") || strings.Contains(out, "-smp 0") {
		t.Fatalf("a disk saved without memory_gb/cpus reseeded the flags with its zeroes, so qemu is asked for no memory and no vCPUs:\n%s", out)
	}
	if !strings.Contains(out, wantMemory) {
		t.Errorf("qemu was not given the default memory %q:\n%s", wantMemory, out)
	}
	if !strings.Contains(out, wantCPUs) {
		t.Errorf("qemu was not given the default vCPU count %q:\n%s", wantCPUs, out)
	}
}

// The removal echo and its error sibling both carry a stored path, and both
// run AFTER the consent prompt -- so neither is the consent vector, and that
// is exactly why they went unguarded twice. The escape at those call sites
// was reverted in a mutation run and the whole suite stayed green: the
// existing full-flow tests poison a path that never exists, so it lands in
// the skip list and the removal arm never executes with a payload at all.
// This test makes the payload reach that arm: a file that really exists,
// inside a managed directory, in a parent the process cannot write to, so
// os.Remove fails and both halves print.
func TestRemovalEchoAndItsErrorAreBothInert(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	cfgDir := t.TempDir()
	cacheDir := t.TempDir()
	t.Setenv("KAIROS_LAB_CONFIG_DIR", cfgDir)
	t.Setenv("KAIROS_LAB_CACHE_DIR", cacheDir)

	// A real file whose NAME carries the payload. Keep the forged row shaped
	// like the plan's own rows, so a miss would be invisible to a reader.
	locked := filepath.Join(cacheDir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	// No '/' in the payload: this is one filename, not a path. The forged row
	// still imitates a real plan row, which is all the attack needs.
	victim := filepath.Join(locked, "vm.log\n  - everything else (will be KEPT)\x1b[2K\r")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	// Read+execute only: the entry is listable and statable, but unlink fails.
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	store := seedInjectedState(t, func(st *state.State) {
		st.ManagedFiles = []string{victim}
		st.ManagedDirs = append(st.ManagedDirs, cacheDir)
		st.Network.CreatedByKairosLab = false
		st.Network.CleanupRequired = false
	})
	_ = store

	var stdout, stderr bytes.Buffer
	err := Run([]string{"cleanup", "-yes"}, strings.NewReader(""), &stdout, &stderr, "test")

	// The removal must actually have been attempted and failed -- otherwise
	// this test passes without ever exercising the lines it exists to guard.
	if err == nil {
		t.Fatalf("cleanup succeeded; the removal arm never failed, so nothing was exercised.\nstdout:\n%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "remove file") {
		t.Fatalf("error %q is not the removal failure this test needs", err)
	}
	if !strings.Contains(stdout.String(), "Removing file:") {
		t.Fatalf("the removal echo never printed, so the arm was not reached:\n%s", stdout.String())
	}

	assertPlanIsInert(t, stdout.String())
	assertPlanIsInert(t, stderr.String())
	assertPlanIsInert(t, err.Error())
}

// --- network mode ---------------------------------------------------------

// The accepted set has exactly one home, and this is what "exactly one" buys:
// a mode is either in networkModes or it is not, and the flag, the reviewer
// prompt and both rejection messages all ask the same function. The case and
// whitespace rows are a deliberate decision, not an accident of the
// implementation: matching stays exact, the way the display mode and the
// subcommand names are matched, because a folded near-miss would be stored in
// state.json and then compared exactly by internal/vm, whose BuildQEMUCommand
// falls back to user networking for anything it does not recognise. A guest
// silently on the wrong network is worse than a rejected typo.
func TestNetworkModeValid(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"shared", true},
		{"bridged", true},
		{"user", true},
		{"", false},
		{"nonsense", false},
		{"Shared", false},
		{"SHARED", false},
		{" shared", false},
	}
	for _, tc := range cases {
		t.Run("mode="+strconv.Quote(tc.mode), func(t *testing.T) {
			if got := networkModeValid(tc.mode); got != tc.want {
				t.Errorf("networkModeValid(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// The empty string deserves its own reason, separate from the table above.
// reviewVMConfig treats an empty answer at prompt 7 as "leave the mode as it
// is" and prints nothing, and it does that by asking networkModeValid first
// and only then filtering "" out of the rejection branch. If "" ever became
// valid, an empty answer would blank the mode instead of keeping it.
func TestNetworkModeValidRejectsTheEmptyString(t *testing.T) {
	if networkModeValid("") {
		t.Fatal(`networkModeValid("") is true, so an empty answer at the reviewer's prompt would set the mode to ""`)
	}
}

// The membership of networkModes, pinned entry by entry. networkModeValid
// only reports whether a mode is in the slice, so every test that asks it a
// question stays green no matter what the slice contains -- a fourth entry is
// simply a fourth mode they never ask about.
//
// That matters because the slice is now the single gate the two former inline
// checks collapsed into. An entry added by accident, or a typo in one ("tap",
// "brigded"), is accepted by the -network flag, accepted by the config
// reviewer, written to state.json as the run's mode, and then falls through
// the default arm of internal/vm's buildLinux/buildMacOS switch to user
// networking: a VM that boots, looks healthy, and is on a network the user did
// not ask for. That is the outcome the slice's own doc comment argues against
// for case folding, reached through the list instead.
//
// The comparison is ordered, which is stricter than the gate needs but is what
// the rest of the CLI shows the user: this is the order the -network usage
// string and the reviewer's prompt 7 name the modes in, so a reordering that
// leaves those behind is worth a red test too.
func TestNetworkModesAreExactlyTheDocumentedModes(t *testing.T) {
	want := []string{"shared", "bridged", "user"}
	if !slices.Equal(networkModes, want) {
		t.Fatalf("networkModes = %q, want exactly %q; every mode in this slice is one the -network flag and the config reviewer both accept and hand to internal/vm", networkModes, want)
	}
}

// The declared default of the -network flag, asserted end to end rather than
// by re-reading the declaration: the value is observed where the user meets
// it, on the config review's Network row, after a `start` that passed no
// -network at all.
//
// The mode is shared, which is what this milestone moved it to: the host side
// of shared is prepared on both platforms now (vm.PrepareLinuxShared on
// Linux, vmnet-shared on macOS), and it is the mode that asks least of the
// host -- no uplink interface, so it works on a Wi-Fi-only laptop where
// bridged cannot, while still putting the guest on a NAT subnet the host can
// route to, where a user-mode guest has no address at all. It is not a
// second VM this buys: see defaultNetworkMode for what this CLI does when
// asked for one.
//
// -bridge-if is passed for one reason, and it is not the interface. It keeps
// the test off the host: runStart hands this flag's value to
// resolveBridgeUplink, which asks the host for a candidate only when it
// arrives empty, so a non-empty one skips the probe. Without it a run in
// bridged mode shells out to `ip route show default` or `ifconfig` and fails
// wherever the answer is unhelpful -- a container whose only default-route
// device is one of the filtered virtual ones (docker*, br-*, veth*, virbr*,
// cni*, podman*), or a macOS runner with no interface reporting an active
// link -- and runStart returns "no suitable uplink interface found for
// bridged networking" before the config review is ever printed, which looks
// exactly like the default having moved.
//
// Under the shared default the probe is gated out anyway (it asks for
// bridged), so the flag is redundant today and is kept anyway: it costs
// nothing, the value is never used -- the run is cancelled at the Enter
// prompt, long before networking is prepared -- and it is what stops this
// test from becoming host-dependent the moment the default moves back or
// that gate widens. A redundant flag is the cheaper of the two mistakes.
// That shared ignores it is asserted directly, by the "(n/a)" on row 8.
// (Tests that need a KNOWN answer from the probe use
// stubBridgeIfaceCandidates instead; this one needs no interface at all.)
func TestStartWithNoNetworkFlagUsesTheDefaultMode(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	seedStartableState(t, "kairos-disk0")

	const wantMode = "shared"

	var stdout, stderr bytes.Buffer
	// -no-iso and an existing disk keep this out of the ISO resolver, and the
	// single newline answers the review's menu and then runs out, so the
	// "Press Enter to start" read hits EOF and the run is cancelled before
	// anything is created or executed. The error is therefore not what is
	// asserted -- the bytes printed on the way to it are -- but it is kept and
	// reported, because a failure here is usually a run that stopped before
	// the review and the error is the only thing that says where.
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-bridge-if", "kairos-test-uplink0"}, scriptedInput("\n"), &stdout, &stderr, "test")

	want := fmt.Sprintf("  7) Network:      %s\n", wantMode)
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("the config review does not show %q: either the -network flag no longer defaults to %q, or the run ended before the review printed. Run returned %v.\nstdout:\n%s\nstderr:\n%s", want, wantMode, err, stdout.String(), stderr.String())
	}
	// The -bridge-if above is not this run's interface under the default
	// mode, and the review says so rather than echoing a value shared never
	// uses.
	if !strings.Contains(stdout.String(), "  8) Net interface: (n/a)") {
		t.Errorf("row 8 offers an interface under %s, which attaches to none:\n%s", wantMode, stdout.String())
	}
}

// `kairos-lab start -h` is where a user learns which modes exist: the usage
// string on the -network flag is what -h prints, and the only listing a user
// reaches without starting a VM, the other one being the config reviewer's
// prompt. README.md carries a third listing, which names all three modes and
// the default now; it is still not what this test guards, because nothing in
// this suite reads the README and a listing no test reads is one that goes
// stale quietly. Nothing else in this suite reads the usage string, so
// dropping shared from the list -- the obvious edit when reverting or
// rewording -- was previously invisible, and a mode nobody is told about is
// one nobody chooses.
//
// The registered default is asserted from the same line, since flag prints it
// as part of the entry. That is a second and more direct witness than
// TestStartWithNoNetworkFlagUsesTheDefaultMode's trip through the config
// review, and the two are deliberately kept in step: this milestone moved the
// default to shared, and a change that reaches only one of them is a default
// the -h output and the review disagree about.
func TestStartUsageListsEveryNetworkModeAndItsDefault(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

	usage, err := captureOSStderr(t, func() error {
		return Run([]string{"start", "-h"}, strings.NewReader(""), io.Discard, io.Discard, "test")
	})
	// -h is not a flag runStart declares, so the flag package handles it:
	// print the usage, return ErrHelp. Anything else means the run got past
	// parsing and what was captured is not the usage message.
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("start -h returned %v, want flag.ErrHelp; captured:\n%s", err, usage)
	}
	want := `network mode: shared|bridged|user (default "shared")`
	if !strings.Contains(usage, want) {
		t.Fatalf("start -h does not print %q, so either a mode is undocumented or the default moved; got:\n%s", want, usage)
	}
}

// captureOSStderr returns what fn wrote to the process's standard error,
// along with fn's error.
//
// It has to reach for the process-wide file rather than the stderr writer Run
// is handed, because those are not the same destination for a usage message:
// runStart builds its flag set with flag.NewFlagSet and never calls
// SetOutput, so flag.FlagSet.Output() falls through to os.Stderr. Giving the
// flag set the writer instead would be a change to production code made only
// so a test could see it, and the point here is to observe what a user at a
// terminal actually gets.
//
// The swap works because Output() reads the os.Stderr variable at the moment
// it prints rather than at flag-set construction. A temp file rather than an
// os.Pipe means there is no reader to schedule and no way to deadlock on a
// full pipe buffer, whatever the usage message grows to.
func captureOSStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr-*.txt")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	saved := os.Stderr
	// A defer as well as the plain restore below, so a panicking fn still
	// leaves the process as it was found. What that is worth is narrower than
	// it looks and worth stating, because the obvious claim is false: the
	// testing package writes failures and recovered panics to os.Stdout, and
	// the runtime writes an unrecovered panic to fd 2 directly rather than
	// through this variable, so a missed restore would silence neither. What
	// it would break is anything that reads the os.Stderr variable at write
	// time -- a later call to this helper, or production code handed
	// os.Stderr -- which would be writing into a file this function has
	// already closed, in a directory t.TempDir removes when the test ends.
	defer func() { os.Stderr = saved }()
	os.Stderr = f
	fnErr := fn()
	os.Stderr = saved
	if err := f.Close(); err != nil {
		t.Fatalf("close captured stderr: %v", err)
	}
	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out), fnErr
}

// The no-argument usage is the other place the network modes are written
// down, and the only one a user reaches without already knowing to ask:
// `start -h` is pinned above, but that message exists only for someone who
// guessed the flag was there. Nothing derived this block from the code, which
// is how -network stayed missing from it for the whole life of the flag.
//
// The block builds both of its mode rows from the declarations now, so the
// expectation here is the rendered row in full rather than the same
// derivation read back: an expectation built out of networkModes and
// defaultNetworkMode would agree with whatever those two were changed to.
// This way the row's values, its wording and the column its description
// starts in are all pinned in one place.
func TestUsageListsEveryNetworkModeAndItsDefault(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(nil, strings.NewReader(""), &stdout, io.Discard, "test"); err != nil {
		t.Fatalf("Run with no arguments: %v", err)
	}
	want := "  -network <mode>      Network mode: shared|bridged|user (default shared)"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("usage printed for no arguments does not contain %q, so a mode is undocumented or the default moved; got:\n%s", want, stdout.String())
	}
}

// Every flag the no-argument usage advertises has to be one `start` really
// declares. The two lists are hand-written in different functions, and a
// usage naming a flag that does not exist is worse than one missing a flag:
// it tells the user to type something the flag set then refuses to parse.
// What it compares against is the flag set's own printed usage, so it is the
// declarations being read rather than a third copy of them.
func TestUsageAdvertisesOnlyFlagsStartDeclares(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

	var stdout bytes.Buffer
	if err := Run(nil, strings.NewReader(""), &stdout, io.Discard, "test"); err != nil {
		t.Fatalf("Run with no arguments: %v", err)
	}
	startUsage, err := captureOSStderr(t, func() error {
		return Run([]string{"start", "-h"}, strings.NewReader(""), io.Discard, io.Discard, "test")
	})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("start -h returned %v, want flag.ErrHelp; captured:\n%s", err, startUsage)
	}
	for _, row := range usageBlock(t, stdout.String(), "Start flags:") {
		name := strings.Fields(row)[0]
		// flag prints one declaration per row as the name, then either the
		// value type or, for a bool, nothing at all.
		declared := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `( |$)`)
		if !declared.MatchString(startUsage) {
			t.Fatalf("usage lists %s under \"Start flags:\" but the start flag set does not declare it, so a user who types it gets a parse error; start -h printed:\n%s", name, startUsage)
		}
	}
}

// Both usage lists are tables laid out by padding the first column by hand,
// and nothing but this check holds that column: a name wider than the padding
// pushes its own description right, and one misaligned row is invisible in a
// diff that adds a single string. The two lists are measured together because
// they share one column -- widening it is allowed, widening it for one row is
// not.
func TestUsageDescriptionsShareOneColumn(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(nil, strings.NewReader(""), &stdout, io.Discard, "test"); err != nil {
		t.Fatalf("Run with no arguments: %v", err)
	}
	// A row is two spaces of indent, a name, and the gap that separates the
	// name from its description; the description starts where that gap ends.
	row := regexp.MustCompile(`^ {2}\S.*?\s{2,}(\S)`)
	column, first := -1, ""
	for _, header := range []string{"Commands:", "Start flags:"} {
		for _, line := range usageBlock(t, stdout.String(), header) {
			m := row.FindStringSubmatchIndex(line)
			if m == nil {
				t.Fatalf("usage row %q under %q is not an indented name followed by a padded description", line, header)
			}
			at := m[2]
			if column == -1 {
				column, first = at, line
				continue
			}
			if at != column {
				t.Fatalf("description in %q starts at column %d but %q starts its own at %d; every row in these two lists shares one column", line, at, first, column)
			}
		}
	}
}

// A flag in either usage list is spelled the way a user types it everywhere
// else in this CLI: with one dash.
//
// `reset [--disk name]` said two, while the flag it names is declared -disk,
// the error runReset prints when it gets a positional argument says "remove a
// single disk with -disk", and the README says `-disk <name>`. Go's flag
// package accepts both spellings, so nothing failed -- the cost was a user
// reading two different names for one flag in the same tool.
//
// Only the name column is read. The descriptions are prose and may hold a
// dash pair for their own reasons.
func TestUsageSpellsFlagsWithASingleDash(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(nil, strings.NewReader(""), &stdout, io.Discard, "test"); err != nil {
		t.Fatalf("Run with no arguments: %v", err)
	}
	name := regexp.MustCompile(`^ {2}(\S.*?)\s{2,}\S`)
	for _, header := range []string{"Commands:", "Start flags:"} {
		for _, line := range usageBlock(t, stdout.String(), header) {
			m := name.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("usage row %q under %q is not an indented name followed by a padded description", line, header)
			}
			if strings.Contains(m[1], "--") {
				t.Errorf("usage row %q spells a flag with two dashes; the rest of the CLI uses one", line)
			}
		}
	}
}

// usageBlock returns the rows printed under header, which run until the blank
// line that ends the block. It reads the rendered usage rather than reaching
// into printUsage, because the alignment being checked is a property of the
// bytes the user sees and of nothing else.
func usageBlock(t *testing.T, usage, header string) []string {
	t.Helper()
	var rows []string
	inBlock := false
	for _, line := range strings.Split(usage, "\n") {
		switch {
		case line == header:
			inBlock = true
		case !inBlock:
		case strings.TrimSpace(line) == "":
			if len(rows) == 0 {
				t.Fatalf("usage block %q is empty; got:\n%s", header, usage)
			}
			return rows
		default:
			rows = append(rows, line)
		}
	}
	t.Fatalf("usage has no %q block; got:\n%s", header, usage)
	return nil
}

// Every mode the CLI documents has to get past validation, and nothing else
// may. These stop at requireSetup on a fresh config dir -- the same seam
// TestRunAcceptsSubcommandsWithoutPositionalArguments uses -- which is proof
// they cleared the mode check without a VM, a disk or a host network being
// touched.
//
// All three modes, on every GOOS, reach exactly that seam. shared used to be
// the exception on Linux, where a temporary refusal sat between the mode
// check and requireSetup because nothing prepared its host side; runStart now
// calls vm.PrepareLinuxShared, so there is no mode the CLI documents and then
// turns away.
func TestStartAcceptsEveryDocumentedNetworkMode(t *testing.T) {
	for _, mode := range []string{"shared", "bridged", "user"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

			var stdout, stderr bytes.Buffer
			err := Run([]string{"start", "-network", mode, "-iso", "/tmp/kairos.iso"}, strings.NewReader(""), &stdout, &stderr, "test")
			if !errors.Is(err, errSetupRequired) {
				t.Fatalf("-network %s: got %v, want %v", mode, err, errSetupRequired)
			}
		})
	}
}

// The rejection is user-facing text and is asserted whole: "invalid network
// mode: %s" is what the CLI has always said, and the value is echoed back so
// the user can see the typo. The case variants are here to pin the exactness
// decision at the CLI boundary too, not only on the helper.
func TestStartRejectsAnUnknownNetworkMode(t *testing.T) {
	for _, mode := range []string{"nonsense", "Shared", "SHARED"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

			var stdout, stderr bytes.Buffer
			err := Run([]string{"start", "-network", mode}, strings.NewReader(""), &stdout, &stderr, "test")
			if err == nil {
				t.Fatalf("-network %s was accepted, want an error", mode)
			}
			want := "invalid network mode: " + mode
			if err.Error() != want {
				t.Fatalf("got %q, want %q", err.Error(), want)
			}
		})
	}
}

// The issue driving this work: a mode the flag accepts that the reviewer does
// not is a mode nobody who opens the config review can keep. Both halves are
// checked here -- the mode is actually changed, and the prompt offers all
// three by name, since a prompt that still reads "(bridged or user)" is how a
// user learns the set.
//
// There is no GOOS skip here, and that is the assertion: the reviewer used to
// refuse shared on Linux while the flag accepted it, because nothing prepared
// the mode's host side there. runStart calls vm.PrepareLinuxShared now, so the
// flag and the reviewer accept the same three modes on every host -- which is
// what this test is really about, the two writers of the mode agreeing.
func TestReviewVMConfigAcceptsSharedNetworkMode(t *testing.T) {
	cfg := reviewableConfig(t)
	cfg.NetworkMode = "bridged"

	var stdout bytes.Buffer
	got, err := reviewVMConfig(cfg, scriptedInput("7\nshared\n\n"), &stdout)
	if err != nil {
		t.Fatalf("reviewVMConfig: %v", err)
	}
	if got.NetworkMode != "shared" {
		t.Errorf("NetworkMode = %q, want %q", got.NetworkMode, "shared")
	}
	if !strings.Contains(stdout.String(), "Enter network mode (shared, bridged or user)") {
		t.Errorf("the prompt does not offer shared, so the user cannot discover it:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), invalidNetworkModeMessage) {
		t.Errorf("a valid mode was rejected:\n%s", stdout.String())
	}
}

// invalidNetworkModeMessage is the reviewer's rejection line. It names all
// three modes: the message is the only place a user who typed a typo is told
// what the alternatives are.
const invalidNetworkModeMessage = "Invalid network mode, use 'shared', 'bridged' or 'user'"

func TestReviewVMConfigRejectsAnUnknownNetworkMode(t *testing.T) {
	cfg := reviewableConfig(t)
	cfg.NetworkMode = "bridged"

	var stdout bytes.Buffer
	got, err := reviewVMConfig(cfg, scriptedInput("7\nnonsense\n\n"), &stdout)
	if err != nil {
		t.Fatalf("reviewVMConfig: %v", err)
	}
	// Unchanged, not blanked and not set to the typo: a rejected answer must
	// leave the configuration the user already had.
	if got.NetworkMode != "bridged" {
		t.Errorf("NetworkMode = %q after a rejected answer, want it left at %q", got.NetworkMode, "bridged")
	}
	if !strings.Contains(stdout.String(), invalidNetworkModeMessage) {
		t.Errorf("the rejection does not list all three modes; got:\n%s", stdout.String())
	}
}

// Pressing Enter at prompt 7 means "I did not want to change this". It has to
// stay silent: printing a rejection for an answer the user never gave teaches
// them that Enter is an error, when it is the way out of the sub-prompt.
func TestReviewVMConfigLeavesTheModeAloneOnAnEmptyAnswer(t *testing.T) {
	cfg := reviewableConfig(t)
	cfg.NetworkMode = "bridged"

	var stdout bytes.Buffer
	got, err := reviewVMConfig(cfg, scriptedInput("7\n\n\n"), &stdout)
	if err != nil {
		t.Fatalf("reviewVMConfig: %v", err)
	}
	if got.NetworkMode != "bridged" {
		t.Errorf("NetworkMode = %q after an empty answer, want it unchanged at %q", got.NetworkMode, "bridged")
	}
	if strings.Contains(stdout.String(), invalidNetworkModeMessage) {
		t.Errorf("an empty answer printed a rejection:\n%s", stdout.String())
	}
}

// shared attaches to no host interface -- on macOS vmnet-shared takes no
// ifname at all, and on Linux the bridge it builds has no uplink to enslave --
// so menu entry 8 has nothing to offer in that mode and says so. The wording
// matters because the old one ("only available for bridged mode") read as a
// platform limitation to a user who had just been given shared by default.
func TestReviewVMConfigHasNoInterfaceToPickInSharedMode(t *testing.T) {
	cfg := reviewableConfig(t)
	cfg.NetworkMode = "shared"
	cfg.NetworkIface = "eth0"

	var stdout bytes.Buffer
	got, err := reviewVMConfig(cfg, scriptedInput("8\n\n"), &stdout)
	if err != nil {
		t.Fatalf("reviewVMConfig: %v", err)
	}
	want := "Invalid option (a network interface applies to bridged mode only, on Linux and macOS; shared mode attaches to no host interface)"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("entry 8 under shared does not explain itself; want %q, got:\n%s", want, stdout.String())
	}
	// Nothing was prompted for, so nothing may have been stored either.
	if got.NetworkIface != "eth0" {
		t.Errorf("NetworkIface = %q, want it untouched at %q", got.NetworkIface, "eth0")
	}
}

// bridgedIfaceSelectable is the gate that keeps shared out of interface
// detection: runStart only probes the host for an uplink when the mode is
// bridged, and the reviewer only offers entry 8 when this returns true. It was
// already correct before shared existed, and adding a mode is exactly the kind
// of change that would quietly widen it -- a gate that answered true for
// shared would send a shared start looking for an uplink it does not use, and
// fail on a Wi-Fi-only host with an error about bridged networking.
func TestBridgedIfaceSelectableOnlyForBridged(t *testing.T) {
	for _, mode := range []string{"shared", "user", "", "nonsense"} {
		if bridgedIfaceSelectable(mode) {
			t.Errorf("bridgedIfaceSelectable(%q) is true, so %q would reach interface detection", mode, mode)
		}
	}
	// The other half: the gate still opens for the mode that needs it,
	// otherwise the assertions above would pass with the function hardwired to
	// false.
	wantBridged := runtime.GOOS == "linux" || runtime.GOOS == "darwin"
	if got := bridgedIfaceSelectable("bridged"); got != wantBridged {
		t.Errorf("bridgedIfaceSelectable(%q) = %v, want %v on %s", "bridged", got, wantBridged, runtime.GOOS)
	}
}

// --- display mode ----------------------------------------------------------

// The accepted set of display modes has one home now, and these are the
// questions it answers: the check in runStart, the reviewer's prompt 9, the
// flag's usage string and the usage block all read displayModes.
//
// The case and whitespace rows are the same decision the network modes make.
// A folded near-miss would be carried into state.json and handed to
// internal/vm, whose two builders switch on the mode by name and return
// "invalid display mode" for anything else -- so the near-miss fails there,
// after the config review and in the middle of a start, instead of at the
// flag the user typed it on.
func TestDisplayModeValid(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"window", true},
		{"serial", true},
		{"", false},
		{"nonsense", false},
		{"Window", false},
		{"SERIAL", false},
		{" serial", false},
	}
	for _, tc := range cases {
		t.Run("mode="+strconv.Quote(tc.mode), func(t *testing.T) {
			if got := displayModeValid(tc.mode); got != tc.want {
				t.Errorf("displayModeValid(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// The empty string, for the same reason it gets its own test on the network
// side: reviewVMConfig treats an empty answer at prompt 9 as "leave the
// display mode as it is", and it does that by asking displayModeValid first
// and filtering "" out of the rejection branch afterwards. If "" ever became
// valid, pressing Enter there would blank the mode instead of keeping it --
// and internal/vm reads a blank mode as serial, so a user who pressed Enter
// would lose the window they were about to get.
func TestDisplayModeValidRejectsTheEmptyString(t *testing.T) {
	if displayModeValid("") {
		t.Fatal(`displayModeValid("") is true, so an empty answer at the reviewer's prompt would set the display mode to ""`)
	}
}

// The membership of displayModes, pinned entry by entry, for the reason
// TestNetworkModesAreExactlyTheDocumentedModes gives: every other test that
// asks displayModeValid a question stays green whatever this slice holds, so
// an entry added or misspelled here is a mode the flag accepts, the reviewer
// accepts, and internal/vm then refuses at the command builder.
//
// The comparison is ordered, because this slice is the order the user is
// shown: both usage listings are built from it.
func TestDisplayModesAreExactlyTheDocumentedModes(t *testing.T) {
	want := []string{"window", "serial"}
	if !slices.Equal(displayModes, want) {
		t.Fatalf("displayModes = %q, want exactly %q; every mode in this slice is one the -display flag and the config reviewer both accept and hand to internal/vm", displayModes, want)
	}
}

// Every display mode the CLI documents gets past validation, and nothing else
// does. These stop at requireSetup on a fresh config dir, which is proof they
// cleared the mode check without a VM or a disk being touched.
func TestStartAcceptsEveryDocumentedDisplayMode(t *testing.T) {
	for _, mode := range displayModes {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

			var stdout, stderr bytes.Buffer
			err := Run([]string{"start", "-display", mode, "-iso", "/tmp/kairos.iso"}, strings.NewReader(""), &stdout, &stderr, "test")
			if !errors.Is(err, errSetupRequired) {
				t.Fatalf("-display %s: got %v, want %v", mode, err, errSetupRequired)
			}
		})
	}
}

// The rejection is user-facing text and is asserted whole, as the network
// one is: the value is echoed back so the user can see their typo.
func TestStartRejectsAnUnknownDisplayMode(t *testing.T) {
	for _, mode := range []string{"nonsense", "Window", "SERIAL"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

			var stdout, stderr bytes.Buffer
			err := Run([]string{"start", "-display", mode}, strings.NewReader(""), &stdout, &stderr, "test")
			if err == nil {
				t.Fatalf("-display %s was accepted, want an error", mode)
			}
			want := "invalid display mode: " + mode
			if err.Error() != want {
				t.Fatalf("got %q, want %q", err.Error(), want)
			}
		})
	}
}

// The declared default of the -display flag, observed where the user meets
// it: on the config review's Display row, after a `start` that passed no
// -display at all.
//
// The expected value is written out rather than read from
// defaultDisplayMode, and that is the point of the test. Everything else that
// shows a user the default -- the flag registration, both usage listings --
// is built from that constant now, so a test that also read it would agree
// with any value it was given. This is a witness that says which value it
// is. Flipping the declared default from window to serial used to leave go
// build, go vet and the whole of go test ./... green; this is one of the
// three tests that was missing.
//
// -bridge-if is here for the reason the network default's own test explains
// at length: it keeps the run off the host's routing table if the network
// default ever moves back to bridged. Under the shared default it is
// ignored.
func TestStartWithNoDisplayFlagUsesTheDefaultMode(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	seedStartableState(t, "kairos-disk0")

	const wantMode = "window"

	// One newline answers the review's menu and then runs out, so the "Press
	// Enter to start" read hits EOF and the run is cancelled before anything
	// is created or executed.
	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-bridge-if", "kairos-test-uplink0"}, scriptedInput("\n"), &stdout, &stderr, "test")

	want := fmt.Sprintf("  9) Display:      %s\n", wantMode)
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("the config review does not show %q: either the -display flag no longer defaults to %q, or the run ended before the review printed. Run returned %v.\nstdout:\n%s\nstderr:\n%s", want, wantMode, err, stdout.String(), stderr.String())
	}
}

// `kairos-lab start -h` is the listing a user reaches by guessing the flag is
// there, and it prints the flag's usage string and its registered default.
// Both are built from displayModes and defaultDisplayMode, so the literal
// here is what pins those two values -- the same job
// TestStartUsageListsEveryNetworkModeAndItsDefault does for the network side.
func TestStartUsageListsEveryDisplayModeAndItsDefault(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())

	usage, err := captureOSStderr(t, func() error {
		return Run([]string{"start", "-h"}, strings.NewReader(""), io.Discard, io.Discard, "test")
	})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("start -h returned %v, want flag.ErrHelp; captured:\n%s", err, usage)
	}
	want := `display mode: window|serial (default "window")`
	if !strings.Contains(usage, want) {
		t.Fatalf("start -h does not print %q, so either a mode is undocumented or the default moved; got:\n%s", want, usage)
	}
}

// The no-argument usage is the other listing, and the one a user reaches
// without knowing the flag exists. Its -display row is built from the same
// two declarations, so what this asserts is the rendered row in full: the
// values, the wording, and the column the description starts in.
//
// Deriving the expectation from displayModes and defaultDisplayMode would be
// the weaker test now that the row itself is derived -- it would hold for
// whatever those two said.
func TestUsageListsEveryDisplayModeAndItsDefault(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run(nil, strings.NewReader(""), &stdout, io.Discard, "test"); err != nil {
		t.Fatalf("Run with no arguments: %v", err)
	}
	want := "  -display <mode>      Display mode: window|serial (default window)"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("usage printed for no arguments does not contain %q, so a mode is undocumented or the default moved; got:\n%s", want, stdout.String())
	}
}

// The reviewer's prompt 9 answers to the same set as the flag. It used to
// compare against two inline string literals, which is one more copy of the
// set than there are modes in it.
func TestReviewVMConfigDisplayModePrompt(t *testing.T) {
	cases := []struct {
		name     string
		answer   string
		want     string
		rejected bool
	}{
		{"a documented mode is taken", "serial", "serial", false},
		{"the mode already set is taken back", "window", "window", false},
		{"an unknown mode is rejected and changes nothing", "nonsense", "window", true},
		{"a near-miss is rejected too", "Serial", "window", true},
		{"an empty answer leaves it alone, silently", "", "window", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := reviewableConfig(t)
			cfg.Display = "window"

			var stdout bytes.Buffer
			got, err := reviewVMConfig(cfg, scriptedInput("9\n"+tc.answer+"\n\n"), &stdout)
			if err != nil {
				t.Fatalf("reviewVMConfig: %v", err)
			}
			if got.Display != tc.want {
				t.Errorf("Display = %q, want %q", got.Display, tc.want)
			}
			if rejected := strings.Contains(stdout.String(), invalidDisplayModeMessage); rejected != tc.rejected {
				t.Errorf("rejection printed = %v, want %v; got:\n%s", rejected, tc.rejected, stdout.String())
			}
		})
	}
}

// invalidDisplayModeMessage is the reviewer's rejection line: the only place
// a user who typed a typo at prompt 9 is told what the alternatives are.
const invalidDisplayModeMessage = "Invalid display mode, use 'window' or 'serial'"

// --- shared networking wiring ---------------------------------------------

// stubNetworkPrivilege puts a recording function in the requireNetworkPrivilege
// seam for the duration of one test and returns the modes it was asked about,
// in order.
//
// The seam is what makes the pre-flight observable at all on this CI leg. The
// real vm.RequireNetworkPrivilege is a no-op everywhere except darwin, so a
// Linux run cannot tell a wired call from a missing one, and every assertion
// below -- which mode is asked about, and what a refusal stops -- would pass
// against a tree that never called it. The restore is a t.Cleanup rather than
// a defer because these tests drive Run rather than the function itself, and
// the package var is shared with every test that follows.
func stubNetworkPrivilege(t *testing.T, answer func(mode string) error) *[]string {
	t.Helper()
	saved := requireNetworkPrivilege
	t.Cleanup(func() { requireNetworkPrivilege = saved })
	calls := &[]string{}
	requireNetworkPrivilege = func(mode string) error {
		*calls = append(*calls, mode)
		return answer(mode)
	}
	return calls
}

// isolateFromHostBinaries points PATH at a directory that does not exist, so
// every exec.Command a run makes fails to find its binary.
//
// That is what keeps the starts below both host-independent and harmless. A
// machine with qemu-img installed would have the disk tests write a real
// image; a machine with qemu-system-* installed would have runStart launch an
// actual VM and then sit in command.Wait() until something killed it. Neither
// outcome is about the code under test. The lookups that fail because of this
// are all ones the code already treats as "no answer" -- df in freeSpaceGB, ip
// in the uplink detection -- and the tests here never take a path that needs
// one.
func isolateFromHostBinaries(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", filepath.Join(t.TempDir(), "no-binaries-here"))
}

// stubFirmwareLookupAway is the companion to isolateFromHostBinaries above,
// and it is needed by every test that calls it and expects a `start` to reach
// anything past the firmware precondition.
//
// runStart resolves firmware early, before the disk and before the networking
// block, and on darwin firmwarePathFor takes the macOSFirmwarePath branch
// unconditionally -- which shells to `brew --prefix qemu`. That is a binary
// isolateFromHostBinaries has just hidden, so on the macos-latest leg the run
// ends at "discover qemu brew prefix" with nothing after the config review
// ever printed. On the ubuntu leg the same test reaches the end of the run,
// so the whole class of breakage is invisible to a Linux-only check.
//
// Reporting a linux/amd64 pair puts firmwarePathFor on its default case,
// where it looks up no firmware at all. That is the narrowest possible
// neutralization: firmwareHostPlatform feeds this one decision and nothing
// else (see its own doc comment), so the network-mode resolution, the sudo
// prompt and every other read of the host below still see the real darwin.
func stubFirmwareLookupAway(t *testing.T) {
	t.Helper()
	saved := firmwareHostPlatform
	t.Cleanup(func() { firmwareHostPlatform = saved })
	firmwareHostPlatform = func() (string, string) { return "linux", "amd64" }
}

// localISO writes a file that `start -iso` accepts. The resolver checks the
// extension and stats the path; the contents are never read.
func localISO(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kairos.iso")
	if err := os.WriteFile(path, []byte("not an iso, never read\n"), 0o644); err != nil {
		t.Fatalf("write iso: %v", err)
	}
	return path
}

// cacheSubdir is the path a run builds under the cache root, resolved through
// the store rather than joined onto KAIROS_LAB_CACHE_DIR by hand: the store
// puts everything in a "kairos-lab" directory inside that root, so a path
// built from the environment variable directly names a directory no run ever
// creates -- and an assertion that a file is absent from it passes without
// having looked anywhere the file could be.
func cacheSubdir(t *testing.T, parts ...string) string {
	t.Helper()
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	return filepath.Join(append([]string{store.CacheDir}, parts...)...)
}

// loadStoredState reads back the state.json a run under test wrote.
func loadStoredState(t *testing.T) *state.State {
	t.Helper()
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return st
}

// soleVM returns the one VM record st carries, failing the test if there is
// not exactly one. Every test in this file that predates multi-VM support
// (M4) drives a single start and wants exactly this record -- the same one
// it used to read through the old, singular st.VM field.
func soleVM(t *testing.T, st *state.State) state.VM {
	t.Helper()
	if len(st.VMs) != 1 {
		t.Fatalf("state has %d VMs, want exactly 1: %#v", len(st.VMs), st.VMs)
	}
	return st.VMs[0]
}

// The privilege pre-flight is asked about the run's mode, and its refusal ends
// the run before anything has been built.
//
// "Before anything" is the whole point of where the call sits, and it is what
// is asserted here: the disk is materialized a few lines further down, so a
// refusal arriving any later would have created the image -- 60 GB by default
// -- for a VM that was never going to start, and on macOS the user would first
// have answered a sudo password prompt that QEMU was going to reject anyway.
//
// All three modes are driven through it because which of them needs root is
// vm.RequireNetworkPrivilege's decision and not this package's: it is
// darwin-only and covers exactly shared and bridged. runStart asks about
// whatever mode the run settled on and does not second-guess the answer, so a
// mode gate added here -- "only ask for shared and bridged" -- would be a
// second copy of that decision, free to drift from the real one.
func TestStartRefusesAModeThisHostCannotPrivilege(t *testing.T) {
	for _, mode := range []string{"shared", "bridged", "user"} {
		t.Run(mode, func(t *testing.T) {
			cacheDir := t.TempDir()
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", cacheDir)
			isolateFromHostBinaries(t)
			seedStartableState(t, "kairos-disk0")

			refused := errors.New("this host cannot privilege " + mode)
			calls := stubNetworkPrivilege(t, func(string) error { return refused })

			// A disk name that does not exist yet, so the run is one that
			// WOULD create an image. -bridge-if keeps the bridged row off
			// the host's routing table, the way
			// TestStartWithNoNetworkFlagUsesTheDefaultMode does.
			var stdout, stderr bytes.Buffer
			err := Run([]string{
				"start", "-name", "kairos-fresh-disk", "-iso", localISO(t),
				"-network", mode, "-bridge-if", "kairos-test-uplink0", "-yes",
			}, strings.NewReader(""), &stdout, &stderr, "test")

			if !errors.Is(err, refused) {
				t.Fatalf("start returned %v, want the pre-flight's own refusal; stdout:\n%s", err, stdout.String())
			}
			if want := []string{mode}; !slices.Equal(*calls, want) {
				t.Errorf("the pre-flight was asked %q, want exactly %q -- once, about the mode this run chose", *calls, want)
			}
			if strings.Contains(stdout.String(), "Creating disk:") {
				t.Errorf("the run announced a disk before the privilege check refused it:\n%s", stdout.String())
			}
			diskPath := cacheSubdir(t, "vm", "kairos-fresh-disk.qcow2")
			if _, statErr := os.Stat(diskPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("os.Stat(%q) = %v, want the image never to have been created", diskPath, statErr)
			}
		})
	}
}

// The other half of the test above, and the reason it means anything: with the
// pre-flight satisfied the very next thing the run does is create the disk. So
// the image missing up there is the refusal stopping it, not the run having
// died of something else before it ever got near.
func TestStartCreatesTheDiskOnceThePrivilegeCheckHasPassed(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")

	// This test is about the privilege check, not firmware, and runStart
	// resolves firmware as a precondition before disk creation. On macos-latest
	// that shells to a brew isolateFromHostBinaries has hidden, which would fail
	// this run one step earlier than the assertion below expects.
	stubFirmwareLookupAway(t)

	calls := stubNetworkPrivilege(t, func(string) error { return nil })

	var stdout, stderr bytes.Buffer
	err := Run([]string{
		"start", "-name", "kairos-fresh-disk", "-iso", localISO(t),
		"-network", "shared", "-yes",
	}, strings.NewReader(""), &stdout, &stderr, "test")

	// qemu-img is where this run stops, because PATH has nothing on it. That
	// is one step past the assertion: the announcement below is printed
	// immediately before vm.EnsureDisk is called.
	if err == nil || !strings.Contains(err.Error(), "create disk image") {
		t.Fatalf("start returned %v, want it to have reached vm.EnsureDisk; stdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Creating disk: kairos-fresh-disk") {
		t.Errorf("the run never reached disk creation, so the refusal test proves nothing:\n%s", stdout.String())
	}
	if want := []string{"shared"}; !slices.Equal(*calls, want) {
		t.Errorf("the pre-flight was asked %q, want %q", *calls, want)
	}
}

// The mode the pre-flight is asked about is the one the config review settled
// on, not the one the flag carried in.
//
// This is why the call cannot live next to the -network validation: the review
// is the second writer of the mode, so a user who passed a mode needing no
// privilege and then chose shared at prompt 7 would never be asked about
// shared at all, and would meet the problem as a sudo prompt in the middle of
// a start. Asking in both places is the other wrong answer -- the same refusal
// printed twice -- which is what the call count pins.
func TestStartPrivilegeCheckUsesTheModeTheReviewSettledOn(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")

	refused := errors.New("this host cannot privilege the mode")
	calls := stubNetworkPrivilege(t, func(string) error { return refused })

	// -network user needs no privilege on any host; "shared" is typed at the
	// review's prompt 7, and the trailing empty line leaves the menu.
	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "user"},
		scriptedInput("7\nshared\n\n"), &stdout, &stderr, "test")

	if !errors.Is(err, refused) {
		t.Fatalf("start returned %v, want the pre-flight's own refusal; stdout:\n%s", err, stdout.String())
	}
	if want := []string{"shared"}; !slices.Equal(*calls, want) {
		t.Fatalf("the pre-flight was asked %q, want %q -- either the flag's value was checked instead of the review's, or the check runs at both", *calls, want)
	}
}

// Preparing the host side of Linux networking needs sudo in both modes that
// have one, and the consent prompt has to describe the mode it is actually
// about.
//
// shared's prompt names neither an uplink nor an interface, and that is a
// requirement rather than a wording preference: vm.PrepareLinuxShared builds a
// bridge whose only port is the tap and clears st.Network.BridgeInterface on
// purpose, so a prompt naming eth0 would be asking the user to agree to
// something the mode never does -- and the name it borrowed would come from
// -bridge-if, which is passed here for exactly that trap.
//
// Answering no is the whole run: nothing is prepared, nothing is recorded, and
// no nmcli or ip or sudo is invoked, which is what keeps this test off the
// host.
func TestStartAsksForSudoBeforePreparingLinuxNetworking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the bridge and tap are prepared by NetworkManager, which is Linux-only; on %s the vmnet modes ask for sudo at the QEMU launch instead", runtime.GOOS)
	}
	tests := []struct {
		name       string
		mode       string
		wantPrompt string
		unwanted   []string
	}{
		{
			name:       "shared names the NAT bridge and no uplink",
			mode:       "shared",
			wantPrompt: "shared networking needs sudo to prepare a NAT bridge/tap (no uplink interface is used)",
			// Not the word, anywhere: the -bridge-if value below is the one
			// thing that could put an interface in this prompt.
			unwanted: []string{"uplink:", "kairos-test-uplink0"},
		},
		{
			name:       "bridged still names the uplink it enslaves",
			mode:       "bridged",
			wantPrompt: "bridged networking needs sudo to prepare bridge/tap (uplink: kairos-test-uplink0)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			isolateFromHostBinaries(t)
			seedStartableState(t, "kairos-disk0")
			// This test is about the sudo prompt and not the pre-flight, so
			// the pre-flight is made to say yes rather than left to whatever
			// the host would answer.
			stubNetworkPrivilege(t, func(string) error { return nil })

			// Enter at the config review, Enter at "press Enter to start",
			// then no at the sudo prompt.
			var stdout, stderr bytes.Buffer
			err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", tt.mode, "-bridge-if", "kairos-test-uplink0"},
				scriptedInput("\n\nn\n"), &stdout, &stderr, "test")

			if err == nil || err.Error() != "sudo permission denied" {
				t.Fatalf("start returned %v, want %q; stdout:\n%s", err, "sudo permission denied", stdout.String())
			}
			if !strings.Contains(stdout.String(), tt.wantPrompt) {
				t.Errorf("the sudo prompt is not %q; got:\n%s", tt.wantPrompt, stdout.String())
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(stdout.String(), unwanted) {
					t.Errorf("the %s run mentions %q, which it never uses:\n%s", tt.mode, unwanted, stdout.String())
				}
			}
			// A refused prompt means a refused run: nothing was prepared, so
			// nothing may have been recorded about it either.
			if strings.Contains(stdout.String(), "[2/3] Recording VM state") {
				t.Errorf("the run carried on past the refused sudo prompt:\n%s", stdout.String())
			}
		})
	}
}

// stubPrepareLinuxShared puts a recording double in place of the host-side
// preparation of shared mode, the way stubPrepareLinuxBridge does for
// bridged. The real vm.PrepareLinuxShared issues sudo nmcli commands against
// an active NetworkManager, so a test that let it run would either fail on
// the CI host or build a NAT bridge, a tap, a DHCP server and a masquerade
// rule on it.
func stubPrepareLinuxShared(t *testing.T, prepare func(st *state.State, runtimeDir string, index int, siblingLive bool) error) {
	t.Helper()
	saved := prepareLinuxShared
	t.Cleanup(func() { prepareLinuxShared = saved })
	prepareLinuxShared = prepare
}

// Both Linux modes hand their prepare the same two things: the state this run
// loaded, and this run's runtime directory.
//
// The two are asserted together because the shared call had nothing on it at
// all. Replacing `prepareLinuxShared(st, runtimeDir)` with a comment left go
// build, go vet and the whole of go test ./... green -- the run that got
// nearest refused the sudo prompt above the call and stopped there -- so the
// line that gives a Linux guest its network was pinned by nothing, while the
// bridged twin beside it reddened three tests when deleted. Running
// the modes off one table is what keeps them from drifting apart again: a
// third mode with a host side, or a change to what either prepare is handed,
// has one place to be agreed on.
//
// What the state is, rather than merely that one was passed: the prepare sees
// the disk this run is about to boot and the runtime directory already on the
// managed list, both of which runStart put there before the branch. And what
// the prepare writes onto it is what the run goes on to use -- the tap name
// below arrives on the recorded QEMU command line, which is the whole point
// of the call. A deleted call leaves the tap empty, and vm.BuildQEMUCommand
// refuses a tap-backed mode without one.
func TestStartPreparesLinuxNetworkingWithTheRunsStateAndRuntimeDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the bridge and tap are prepared by NetworkManager, which is Linux-only; on %s neither branch is reached", runtime.GOOS)
	}
	cases := []struct {
		mode string
		stub func(*testing.T, func(st *state.State, runtimeDir string, index int, siblingLive bool) error)
	}{
		{"shared", stubPrepareLinuxShared},
		{"bridged", stubPrepareLinuxBridge},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			cacheDir := t.TempDir()
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", cacheDir)
			isolateFromHostBinaries(t)
			stubLinuxARM64Firmware(t)
			seedStartableState(t, "kairos-disk0")
			stubNetworkPrivilege(t, func(string) error { return nil })

			// The tap name is this test's own, so finding it on the command
			// line means it came back from the prepare and from nowhere
			// else: vm.DefaultTapName would also be what a fallback used.
			const tapName = "kairoslab-tap-under-test"
			calls := 0
			var gotRuntimeDir string
			var sawRunsDisk, sawManagedRuntimeDir bool
			tc.stub(t, func(st *state.State, runtimeDir string, index int, siblingLive bool) error {
				calls++
				gotRuntimeDir = runtimeDir
				sawRunsDisk = state.FindDiskByName(st, "kairos-disk0") != nil
				sawManagedRuntimeDir = slices.Contains(st.ManagedDirs, runtimeDir)
				// The fields both real prepares write on success. Only the
				// ones the two share are set here; which uplink each records
				// is their own decision and is asserted where that decision
				// lives (see TestStartRecordsTheUplinkThePrepareUsed).
				st.Network.Mode = tc.mode
				st.Network.BridgeName = vm.DefaultBridgeName
				st.Network.TapName = tapName
				st.Network.CleanupRequired = true
				st.Network.CreatedByKairosLab = true
				return nil
			})

			// -yes consents to the sudo prompt, so the run goes through the
			// prepare and on to the launch, which PATH isolation makes fail.
			// -bridge-if keeps the bridged row off the host's routing table,
			// as TestStartRefusesAModeThisHostCannotPrivilege does; shared
			// ignores it.
			var stdout, stderr bytes.Buffer
			err := Run([]string{
				"start", "-name", "kairos-disk0", "-no-iso",
				"-network", tc.mode, "-bridge-if", "kairos-test-uplink0", "-yes",
			}, strings.NewReader(""), &stdout, &stderr, "test")

			if err == nil || !strings.Contains(err.Error(), "start qemu") {
				t.Fatalf("start returned %v, want it to have prepared the network, recorded the VM and then failed to launch it; stdout:\n%s", err, stdout.String())
			}
			if calls != 1 {
				t.Fatalf("the %s prepare was called %d times, want exactly once: this is the call that builds the bridge and the tap the guest is attached to", tc.mode, calls)
			}
			if want := cacheSubdir(t, "runtime"); gotRuntimeDir != want {
				t.Errorf("the %s prepare was handed runtime dir %q, want %q -- the directory this run creates and records, which is where the tap's owner and the lease file are looked for", tc.mode, gotRuntimeDir, want)
			}
			if !sawRunsDisk {
				t.Errorf("the %s prepare was handed a state without this run's disk, so it is not the state the run loaded", tc.mode)
			}
			if !sawManagedRuntimeDir {
				t.Errorf("the %s prepare was handed a state whose managed dirs do not hold the runtime dir, so it is not the state the run loaded", tc.mode)
			}

			st := loadStoredState(t)
			if st.Network.TapName != tapName {
				t.Errorf("state records tap %q, want %q -- what the prepare reported, which is what the teardown reads", st.Network.TapName, tapName)
			}
			wantNetdev := "tap,id=net0,ifname=" + tapName + ",script=no,downscript=no"
			if !slices.Contains(soleVM(t, st).QemuArgs, wantNetdev) {
				t.Errorf("the recorded qemu command line has no %q, so the guest is not on the tap the prepare built:\n%q", wantNetdev, soleVM(t, st).QemuArgs)
			}
		})
	}
}

// The sudo prompt is a gate and not an announcement: a refusal means the
// prepare never runs.
//
// TestStartAsksForSudoBeforePreparingLinuxNetworking pins what the prompt
// says and that a refusal ends the run, which is everything except the thing
// the prompt is asking about. Both halves are needed, and this is the half
// that would notice a prepare moved above its own consent -- the run would
// still stop with the same error, having already built a NAT bridge on the
// user's host after they said no.
func TestStartDoesNotPrepareSharedNetworkingWithoutConsent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the NAT bridge and tap are prepared by NetworkManager, which is Linux-only; on %s this branch is not reached", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })

	calls := 0
	stubPrepareLinuxShared(t, func(*state.State, string, int, bool) error {
		calls++
		return nil
	})

	// Enter at the config review, Enter at "press Enter to start", then no at
	// the sudo prompt.
	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "shared"},
		scriptedInput("\n\nn\n"), &stdout, &stderr, "test")

	if err == nil || err.Error() != "sudo permission denied" {
		t.Fatalf("start returned %v, want %q; stdout:\n%s", err, "sudo permission denied", stdout.String())
	}
	if calls != 0 {
		t.Fatalf("the shared prepare ran %d times after the user refused sudo, so the consent prompt is decoration: a NAT bridge, a tap and a DHCP server would have been built on the host anyway", calls)
	}
}

// A prepare that fails ends the start, with its own error and before anything
// is recorded.
//
// The failure is the realistic one rather than a contrived one: the real
// vm.PrepareLinuxShared refuses a host whose bridge already carries a port it
// did not put there, and returns rather than starting a VM whose network was
// never built. Carrying on would launch QEMU against a tap that does not
// exist -- and record a state file saying it does.
func TestStartStopsWhenPreparingSharedNetworkingFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the NAT bridge and tap are prepared by NetworkManager, which is Linux-only; on %s this branch is not reached", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })

	failed := errors.New("bridge kairoslab0 already has a port this run did not add")
	stubPrepareLinuxShared(t, func(*state.State, string, int, bool) error { return failed })

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "shared", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test")

	if !errors.Is(err, failed) {
		t.Fatalf("start returned %v, want the prepare's own error; stdout:\n%s", err, stdout.String())
	}
	if strings.Contains(stdout.String(), "[2/3] Recording VM state") {
		t.Errorf("the run recorded a VM whose network was never prepared:\n%s", stdout.String())
	}
	// A failed prepare releases the reservation (M4): nothing about this VM
	// was ever really recorded, so no VM record survives at all -- not one
	// with an empty QemuBinary.
	if st := loadStoredState(t); len(st.VMs) != 0 || st.Network.Mode != "" {
		t.Errorf("state records %d VMs and network mode %q after a failed prepare, want none and empty", len(st.VMs), st.Network.Mode)
	}
}

// TestStartChecksFirmwareBeforePreparingLinuxNetworking pins the ordering
// that fixed kairos-io/kairos-lab#8's sharpest half: on a linux/arm64 host
// with no EDK2 installed, `start -network shared` (and, on this host,
// `-network bridged` just as much) used to prompt for sudo, let the mode's
// own vm.PrepareLinuxShared / vm.PrepareLinuxBridge build a NAT bridge and a
// tap -- setting st.Network.BridgeName, TapName, CleanupRequired and
// CreatedByKairosLab in memory, on the *state.State this run loaded -- and
// only THEN fail on the missing firmware, with store.Save(st) still well
// downstream and never reached. The host kept a privileged bridge and tap
// that state.json had no record of, so neither `kairos-lab reset` nor
// `kairos-lab cleanup` could find them to tear down.
//
// This is a trigger and not a new bug on its own: at 59c6949 the firmware
// `return err` was gated on runtime.GOOS == "darwin", so it was unreachable
// from the linux-gated prepare blocks below it. Making the check
// unconditional put a Linux-reachable early return downstream of two
// Linux-only privileged prepares, not one -- which is why both modes are
// driven through the same table below rather than only shared: the fix sits
// above both branches, and a regression that moved the check back below only
// one of them would otherwise show up in just the row nobody happened to be
// looking at. (Demonstrated: gate the hoisted check on `*network !=
// "bridged"` and restore an equivalent one just above buildQEMUCommand for
// bridged, and the shared-only version of this test stayed green while the
// bridged row here reddens -- see that row's own comment for the message.)
//
// firmwareHostPlatform is stubbed to a linux/arm64 host the real GOOS/GOARCH
// pair on this CI runner is not (see TestFirmwarePathFor's comment on why
// that seam exists at all), and linuxARM64Firmware is pointed at a path that
// does not exist, so firmwarePathFor fails deterministically regardless of
// what is actually installed on the machine running this suite. The
// runtime.GOOS gate below is real and unstubbed, because the prepares this
// pins the check AGAINST are linux-only -- the check itself is unconditional
// -- so on any other host there is no ordering to pin and this skips, as
// TestStartPreparesLinuxNetworkingWithTheRunsStateAndRuntimeDir does.
//
// Swallowing the error (biosPath, _ := firmwarePathFor(...)) also reddens
// this, but by a route worth writing down, because the obvious guess is
// wrong. It does NOT die on buildLinuxFor's arm64 message: only
// firmwareHostPlatform is stubbed, while buildLinux -- one frame below
// vm.BuildQEMUCommand -- binds buildLinuxFor to the real runtime.GOARCH, so
// on the amd64 CI leg it never enters the arm64 case at all. It dies on
// "<mode> linux mode requires tap name" instead -- the message is %s-formatted
// from cfg.NetworkMode, so each row gets its own -- from the stubbed prepare
// leaving TapName empty; caught by the FIRST error-content check below, which
// t.Fatalf's before the remedy-clause check is ever reached. So the swallow
// is caught, but not by the assertion that looks like it would.
func TestStartChecksFirmwareBeforePreparingLinuxNetworking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the firmware check this pins runs everywhere, but the prepares it must run BEFORE -- vm.PrepareLinuxShared and vm.PrepareLinuxBridge -- are Linux-only; on %s there is no ordering left to pin", runtime.GOOS)
	}

	cases := []struct {
		mode        string
		prepareName string
		args        []string
		stub        func(*testing.T, func(st *state.State, runtimeDir string, index int, siblingLive bool) error)
	}{
		{
			mode:        "shared",
			prepareName: "prepareLinuxShared",
			args:        []string{"-network", "shared"},
			stub:        stubPrepareLinuxShared,
		},
		{
			// bridged needs its own scaffolding shared does not: a candidate
			// uplink, so the real bridgeIfaceCandidates -- which probes the
			// host through vm.DetectUplinkCandidates -- never runs, and -yes
			// still covers this mode's own sudo prompt the
			// same way it covers shared's.
			mode:        "bridged",
			prepareName: "prepareLinuxBridge",
			args:        []string{"-network", "bridged"},
			stub:        stubPrepareLinuxBridge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			isolateFromHostBinaries(t)
			seedStartableState(t, "kairos-disk0")
			stubNetworkPrivilege(t, func(string) error { return nil })
			if tc.mode == "bridged" {
				stubBridgeIfaceCandidates(t, "kairos-fake-uplink0")
			}

			savedPlatform := firmwareHostPlatform
			t.Cleanup(func() { firmwareHostPlatform = savedPlatform })
			firmwareHostPlatform = func() (string, string) { return "linux", "arm64" }

			missingFirmware := filepath.Join(t.TempDir(), "no-such-firmware.fd")
			savedFirmware := linuxARM64Firmware
			t.Cleanup(func() { linuxARM64Firmware = savedFirmware })
			linuxARM64Firmware = []string{missingFirmware}

			calls := 0
			tc.stub(t, func(*state.State, string, int, bool) error {
				calls++
				return nil
			})

			var stdout, stderr bytes.Buffer
			args := append([]string{"start", "-name", "kairos-disk0", "-no-iso"}, tc.args...)
			args = append(args, "-yes")
			err := Run(args, strings.NewReader(""), &stdout, &stderr, "test")

			if err == nil || !strings.Contains(err.Error(), "arm64 UEFI firmware not found") {
				t.Fatalf("start returned %v, want linuxARM64FirmwarePath's error; stdout:\n%s", err, stdout.String())
			}
			if !strings.Contains(err.Error(), missingFirmware) {
				t.Errorf("error %q does not name the missing candidate %q", err, missingFirmware)
			}
			if !strings.Contains(err.Error(), "qemu-efi-aarch64") {
				t.Errorf("error %q lost the remedy clause naming the package to install", err)
			}
			// This is the row that catches the ordering hole: gating the
			// hoisted firmware check on `*network != "bridged"` and
			// restoring an equivalent one just above buildQEMUCommand
			// leaves prepareLinuxBridge free to run first, so this call
			// count goes from 0 to 1 under that mutation while the shared
			// row above stays green throughout.
			if calls != 0 {
				t.Fatalf("%s ran %d times before the firmware check failed, want 0 -- the NAT bridge and tap it builds would be left on the host with nothing in state.json to name them, unreachable by reset or cleanup", tc.prepareName, calls)
			}
			if strings.Contains(stdout.String(), "[1/3] Preparing networking") {
				t.Errorf("the run printed the networking step before the firmware check failed:\n%s", stdout.String())
			}
		})
	}
}

// stubBridgeIfaceCandidates answers the uplink probe with a fixed list for
// the duration of one test.
//
// The real bridgeIfaceCandidates shells out -- `ip route show default` on
// Linux, the vmnet interface list on macOS -- so what it returns is whatever
// the machine running the suite is plugged into. A container whose only
// default route is one of the filtered virtual devices answers "nothing", and
// so does a macOS runner with no active link, which is how a test that asks
// the host comes to pass on a laptop and fail on a CI leg. Every assertion
// below is about which interface the run names and records, so the answer has
// to be one this file chose.
func stubBridgeIfaceCandidates(t *testing.T, candidates ...string) {
	t.Helper()
	saved := bridgeIfaceCandidates
	t.Cleanup(func() { bridgeIfaceCandidates = saved })
	bridgeIfaceCandidates = func() []string { return candidates }
}

// stubPrepareLinuxBridge puts a recording double in place of the host-side
// bridge preparation. The real one needs an active NetworkManager and issues
// sudo nmcli commands, so a test that let it run would either fail on the CI
// host or reconfigure it.
func stubPrepareLinuxBridge(t *testing.T, prepare func(st *state.State, runtimeDir string, index int, siblingLive bool) error) {
	t.Helper()
	saved := prepareLinuxBridge
	t.Cleanup(func() { prepareLinuxBridge = saved })
	prepareLinuxBridge = prepare
}

// stubLinuxARM64Firmware points linuxARM64Firmware at a file this test wrote
// itself, for the duration of the test.
//
// linuxARM64FirmwarePath stats absolute /usr/share/... paths, which is host
// state that neither isolateFromHostBinaries nor fakeQEMUOnPath's PATH
// isolation can reach: those change what a child process resolves a bare
// command name to, not what os.Stat finds at a path this file never composed
// from PATH. A run on the linux/arm64 leg of this suite -- or any leg with
// this branch forced on for a manual check -- would otherwise stat the real
// /usr/share/AAVMF and friends, and pass or fail by what happens to be
// installed on the machine running the tests rather than by what the test
// asserts.
//
// It returns the path it wrote, for a caller that wants to assert the
// resolver returned that exact file rather than only that it returned
// without error. Most callers ignore the return value.
func stubLinuxARM64Firmware(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "QEMU_EFI.fd")
	if err := os.WriteFile(path, []byte("firmware"), 0o644); err != nil {
		t.Fatalf("write fake firmware: %v", err)
	}
	saved := linuxARM64Firmware
	t.Cleanup(func() { linuxARM64Firmware = saved })
	linuxARM64Firmware = []string{path}
	return path
}

// A bridged run consents to enslaving a named interface, even when the mode
// was chosen inside the config review rather than on the command line.
//
// This is the pairing the default flip broke. Uplink detection used to be
// keyed on the FLAG's mode and the sudo consent on the REVIEWED one, which
// agreed for exactly as long as the flag defaulted to bridged. Once it
// defaulted to shared, a `start` with no -network that answered "bridged" at
// prompt 7 skipped detection entirely: the prompt read "(uplink: )", and
// answering y handed the job to vm.PrepareLinuxBridge, which detects an
// uplink of its own and enslaves it -- the host's own NIC, named to nobody.
//
// Row 8 of the review is asserted as well as the prompt. They are two
// different resolutions -- the review fills the row in as soon as the mode
// changes, so the user can see and change the interface at entry 8, and
// runStart resolves again on the way out -- and a blank row is how a user
// learns of the interface only from the sudo prompt.
func TestStartNamesTheUplinkWhenTheReviewChoosesBridged(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the bridge and tap are prepared by NetworkManager, which is Linux-only; on %s the vmnet modes ask for sudo at the QEMU launch instead", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t, "kairos-fake-uplink0", "kairos-fake-uplink1")

	// No -network and no -bridge-if: the run arrives in the default mode,
	// picks bridged at prompt 7, leaves the menu, presses Enter to start and
	// then refuses the sudo prompt, so nothing is prepared.
	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso"},
		scriptedInput("7\nbridged\n\n\nn\n"), &stdout, &stderr, "test")

	if err == nil || err.Error() != "sudo permission denied" {
		t.Fatalf("start returned %v, want %q; stdout:\n%s", err, "sudo permission denied", stdout.String())
	}
	out := stdout.String()
	want := "bridged networking needs sudo to prepare bridge/tap (uplink: kairos-fake-uplink0)"
	if !strings.Contains(out, want) {
		t.Errorf("the sudo prompt is not %q; got:\n%s", want, out)
	}
	if strings.Contains(out, "(uplink: )") {
		t.Errorf("the sudo prompt names no interface, so it consents to whatever the prepare detects:\n%s", out)
	}
	if wantRow := "8) Net interface: kairos-fake-uplink0"; !strings.Contains(out, wantRow) {
		t.Errorf("the review does not show %q after the mode became bridged, so the interface is invisible until the sudo prompt:\n%s", wantRow, out)
	}
}

// The other end of the same resolution: a host with no candidate has to stop
// the run, not carry an empty interface into the sudo prompt.
//
// The review cannot raise this error itself -- an error inside the menu
// throws away every other edit made in it -- so it leaves the row empty and
// runStart refuses on the way out. Without that second resolution the run
// reaches the consent prompt with nothing to name, which is the failure
// above wearing a different hat.
func TestStartRefusesBridgedChosenAtTheReviewWithNoUplinkAvailable(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("bridged networking has no host side on %s", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t)

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso"},
		scriptedInput("7\nbridged\n\n\nn\n"), &stdout, &stderr, "test")

	if err == nil || !strings.Contains(err.Error(), "-bridge-if") {
		t.Fatalf("start returned %v, want the refusal naming a way out; stdout:\n%s", err, stdout.String())
	}
	if strings.Contains(stdout.String(), "needs sudo to prepare bridge/tap") {
		t.Errorf("the run asked for sudo before finding out it had no interface to enslave:\n%s", stdout.String())
	}
}

// What is recorded is the interface the prepare actually used.
//
// vm.PrepareLinuxBridge writes the uplink it enslaved onto st.Network, and it
// has a detection path of its own for when the field arrives empty. The
// recording block used to overwrite that answer with a value re-derived from
// the -bridge-if flag, so a run that auto-detected and enslaved a real NIC
// stored an empty interface -- and `status` and the teardown read that field.
// The double below returns a different name from the one the run resolved,
// which is what a detection inside the prepare looks like from here.
func TestStartRecordsTheUplinkThePrepareUsed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("vm.PrepareLinuxBridge only does anything on linux; on %s it returns nil without touching state", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubLinuxARM64Firmware(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t, "kairos-fake-uplink0")

	var asked string
	stubPrepareLinuxBridge(t, func(st *state.State, _ string, _ int, _ bool) error {
		asked = st.Network.BridgeInterface
		// The fields vm.PrepareLinuxBridge writes on success, with an uplink
		// of its own choosing.
		st.Network.Mode = "bridged"
		st.Network.BridgeName = vm.DefaultBridgeName
		st.Network.TapName = vm.DefaultTapName
		st.Network.BridgeInterface = "kairos-prepared-uplink0"
		st.Network.CleanupRequired = true
		st.Network.CreatedByKairosLab = true
		return nil
	})

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "bridged", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test")
	// The state is written at "[2/3] Recording VM state", one step before the
	// launch that PATH isolation makes fail.
	if err == nil || !strings.Contains(err.Error(), "start qemu") {
		t.Fatalf("start returned %v, want it to have recorded the VM and then failed to launch it; stdout:\n%s", err, stdout.String())
	}
	if asked != "kairos-fake-uplink0" {
		t.Errorf("the prepare was asked about %q, want the interface the run resolved and named in its prompt", asked)
	}

	st := loadStoredState(t)
	if st.Network.BridgeInterface != "kairos-prepared-uplink0" {
		t.Errorf("state records uplink %q, want %q -- the one the prepare enslaved, which is what `status` and the teardown read",
			st.Network.BridgeInterface, "kairos-prepared-uplink0")
	}
	if st.Network.Mode != "bridged" {
		t.Errorf("state records mode %q, want %q", st.Network.Mode, "bridged")
	}
	// And the run really did go through the bridged arm: the guest is on the
	// tap the prepare reported, not on user networking.
	wantNetdev := "tap,id=net0,ifname=" + vm.DefaultTapName + ",script=no,downscript=no"
	if !slices.Contains(soleVM(t, st).QemuArgs, wantNetdev) {
		t.Errorf("the recorded qemu command line has no %q:\n%q", wantNetdev, soleVM(t, st).QemuArgs)
	}
}

// resolveBridgeUplink is the single answer to "which interface does a bridged
// run attach to", and it is asked twice: once against the mode the -network
// flag carried in, once against the mode the config review settled on. One
// call is not enough, because those are different values -- that is the whole
// bug -- and two calls only work because the second is a no-op once the first
// has answered.
func TestResolveBridgeUplink(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("bridged networking has no host side on %s", runtime.GOOS)
	}
	cases := []struct {
		name       string
		mode       string
		iface      string
		candidates []string
		want       string
		wantErr    bool
	}{
		{"bridged with nothing chosen takes the first candidate", "bridged", "", []string{"eth0", "eth1"}, "eth0", false},
		{"bridged keeps what the user chose", "bridged", "eth9", []string{"eth0"}, "eth9", false},
		{"bridged with no candidate is an error, not an empty answer", "bridged", "", nil, "", true},
		{"shared asks the host nothing", "shared", "", nil, "", false},
		{"user asks the host nothing", "user", "", nil, "", false},
		{"shared keeps -bridge-if for bridgeInterfaceForMode to drop", "shared", "eth9", nil, "eth9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubBridgeIfaceCandidates(t, tc.candidates...)
			got, err := resolveBridgeUplink(tc.mode, tc.iface)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveBridgeUplink(%q, %q) error = %v, want error: %v", tc.mode, tc.iface, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("resolveBridgeUplink(%q, %q) = %q, want %q", tc.mode, tc.iface, got, tc.want)
			}
		})
	}
}

// Both platforms' "no interface to attach to" messages, pinned from either CI
// leg. The sentences differ because the failures do -- no default route
// through anything physical on Linux, no interface reporting a link on macOS
// -- and each has to carry the two ways out, since a user who reads it on the
// platform it belongs to has no other listing of them.
func TestNoBridgeUplinkErrorSpeaksForEachPlatform(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{"linux", "no suitable uplink interface found for bridged networking (use -bridge-if to specify one, or -network user for port-forwarded access)"},
		{"darwin", "no host interface has a link, so bridged networking would leave the VM without an address (use -bridge-if to specify one, or -network user for port-forwarded access)"},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			err := noBridgeUplinkError(tc.goos)
			if err == nil || err.Error() != tc.want {
				t.Errorf("noBridgeUplinkError(%q) = %v, want %q", tc.goos, err, tc.want)
			}
		})
	}
}

// Launching QEMU itself as root is a macOS-only thing, and it covers BOTH
// vmnet modes.
//
// shared is -netdev vmnet-shared and needs root exactly as vmnet-bridged
// does; launched unprivileged, QEMU exits with a vmnet error and no VM. The
// pair is the same one vm.RequireNetworkPrivilege's darwinRootModes names, so
// the two agree about which modes need root. On Linux nothing here runs as
// root: the bridge and tap are prepared beforehand and QEMU opens a tap that
// already belongs to the user.
//
// goos is a parameter for the sake of this table. The branch in runStart is
// GOOS-gated, so narrowing it back to bridged alone -- which is what it said
// before shared was wired up -- survived the whole suite on the Linux leg.
func TestVmnetNeedsSudo(t *testing.T) {
	cases := []struct {
		goos string
		mode string
		want bool
	}{
		{"darwin", "bridged", true},
		{"darwin", "shared", true},
		{"darwin", "user", false},
		{"darwin", "", false},
		{"linux", "bridged", false},
		{"linux", "shared", false},
		{"linux", "user", false},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.mode, func(t *testing.T) {
			if got := vmnetNeedsSudo(tc.goos, tc.mode); got != tc.want {
				t.Errorf("vmnetNeedsSudo(%q, %q) = %v, want %v", tc.goos, tc.mode, got, tc.want)
			}
		})
	}
}

// And what that consent says, which is the other half nothing could see: the
// prompt is printed from the same GOOS-gated branch, so replacing it wholesale
// survived the suite too. It names the mode, because that is what tells the
// user which of their two vmnet choices is about to run as root.
func TestVmnetSudoPromptNamesTheMode(t *testing.T) {
	cases := map[string]string{
		"bridged": "bridged vmnet mode runs qemu with sudo",
		"shared":  "shared vmnet mode runs qemu with sudo",
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			if got := vmnetSudoPrompt(mode); got != want {
				t.Errorf("vmnetSudoPrompt(%q) = %q, want %q", mode, got, want)
			}
		})
	}
}

// bridgeInterfaceForMode is the single answer to "what host interface does
// this run attach to", and both places runStart records one ask it: the
// st.Network.BridgeInterface field that `status` prints and the teardown
// reads, and vm.StartConfig.BridgeIface, whose own doc says it "is meaningful
// only for bridged mode".
//
// Only bridged attaches to an interface. -bridge-if is accepted by the flag
// set whatever the mode, so `start -network shared -bridge-if eth0` has to
// drop the value rather than record an uplink this VM never used and that
// nothing later can tell apart from a real one.
func TestBridgeInterfaceForModeAnswersOnlyForBridged(t *testing.T) {
	cases := []struct {
		mode  string
		iface string
		want  string
	}{
		{"bridged", "eth0", "eth0"},
		{"shared", "eth0", ""},
		{"user", "eth0", ""},
		{"", "eth0", ""},
		{"nonsense", "eth0", ""},
		{"bridged", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.iface, func(t *testing.T) {
			if got := bridgeInterfaceForMode(tc.mode, tc.iface); got != tc.want {
				t.Errorf("bridgeInterfaceForMode(%q, %q) = %q, want %q", tc.mode, tc.iface, got, tc.want)
			}
		})
	}
}

// The same decision reached through a whole start, so that the helper above is
// pinned at the call sites and not only on its own.
//
// user mode is what this drives because it is the only one of the three that
// reaches the state save without touching the host: the shared and bridged
// arms both prepare a NetworkManager bridge through sudo first. What holds for
// user holds for shared by construction -- one function answers for both, and
// the table above covers the other rows.
func TestStartRecordsNoUplinkForAModeThatAttachesToNone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test asserts the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it records anything, and this test's isolation from host binaries denies it one", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubLinuxARM64Firmware(t)
	seedStartableState(t, "kairos-disk0")

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "user", "-bridge-if", "kairos-test-uplink0", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test")
	// The state is written at "[2/3] Recording VM state", one step before the
	// launch that PATH isolation makes fail.
	if err == nil || !strings.Contains(err.Error(), "start qemu") {
		t.Fatalf("start returned %v, want it to have recorded the VM and then failed to launch it; stdout:\n%s", err, stdout.String())
	}

	st := loadStoredState(t)
	if st.Network.Mode != "user" {
		t.Fatalf("state records mode %q, want %q", st.Network.Mode, "user")
	}
	if st.Network.BridgeInterface != "" {
		t.Errorf("state records an uplink of %q for a run that attached to none; `status` would report it as this VM's interface", st.Network.BridgeInterface)
	}
	for _, arg := range soleVM(t, st).QemuArgs {
		if strings.Contains(arg, "kairos-test-uplink0") {
			t.Errorf("the qemu command line carries the interface: %q", arg)
		}
	}
}

// Every disk gets its own guest NIC address, and keeps it.
//
// QEMU seeds every process with the same default address, so two VMs on one
// subnet collide and a DHCP lease cannot be attributed to either. vm.MACForDisk
// hashes the disk name, which gives each VM its own address AND the same
// address on every restart -- the second half is what keeps a recorded lease
// from going stale under the VM that holds it.
//
// A disk that already carries an address keeps it untouched. That is what
// makes the field sticky rather than derived: a user who edited it, or a
// future version that assigns addresses some other way, is not overwritten on
// the next start, and a disk recorded before the field existed simply gets one
// filled in.
func TestStartGivesEachDiskItsOwnStickyMAC(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test asserts the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it records anything, and this test's isolation from host binaries denies it one", runtime.GOOS)
	}
	const diskName = "kairos-disk0"
	const storedMAC = "52:54:00:ab:cd:ef"
	tests := []struct {
		name    string
		seedMAC string
		want    string
	}{
		{"derived when the disk carries none", "", vm.MACForDisk(diskName)},
		{"kept when the disk already carries one", storedMAC, storedMAC},
		// Whitespace is not an address, and the check here has to agree with
		// internal/vm about that. netDeviceArg trims before deciding a value
		// is unset, so a stored "   " that got past an untrimmed check here
		// was handed to QEMU as a bare device with no mac= at all: the guest
		// took QEMU's single default address -- the collision the derived one
		// exists to prevent -- and the whitespace was persisted, so the next
		// start did it again.
		{"derived when the stored address is only whitespace", "   ", vm.MACForDisk(diskName)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			isolateFromHostBinaries(t)
			stubLinuxARM64Firmware(t)
			seedStartableState(t, diskName)
			if tt.seedMAC != "" {
				store, err := state.DefaultStore()
				if err != nil {
					t.Fatalf("DefaultStore: %v", err)
				}
				st, err := store.Load()
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				state.FindDiskByName(st, diskName).MAC = tt.seedMAC
				if err := store.Save(st); err != nil {
					t.Fatalf("Save: %v", err)
				}
			}

			// user mode, because it is the one that reaches the QEMU command
			// line without preparing anything on the host. The address is
			// attached to the NIC device, which is built once for all three
			// modes in internal/vm.
			var stdout, stderr bytes.Buffer
			err := Run([]string{"start", "-name", diskName, "-no-iso", "-network", "user", "-yes"},
				strings.NewReader(""), &stdout, &stderr, "test")
			if err == nil || !strings.Contains(err.Error(), "start qemu") {
				t.Fatalf("start returned %v, want it to have recorded the VM and then failed to launch it; stdout:\n%s", err, stdout.String())
			}

			st := loadStoredState(t)
			disk := state.FindDiskByName(st, diskName)
			if disk == nil {
				t.Fatalf("the disk is gone from state:\n%+v", st.Disks)
			}
			if disk.MAC != tt.want {
				t.Errorf("state records MAC %q for %s, want %q", disk.MAC, diskName, tt.want)
			}
			// Persisting it and using it are different failures: a MAC
			// written to state but not handed to QEMU leaves every VM on the
			// colliding default address while state.json claims otherwise.
			wantArg := "virtio-net-pci,netdev=net0,mac=" + tt.want
			if !slices.Contains(soleVM(t, st).QemuArgs, wantArg) {
				t.Errorf("the recorded qemu command line has no %q:\n%q", wantArg, soleVM(t, st).QemuArgs)
			}
			if !strings.Contains(stdout.String(), wantArg) {
				t.Errorf("the command printed to the user has no %q:\n%s", wantArg, stdout.String())
			}
		})
	}
}

// The address is derived from the name the review settled on, not from the
// one the run started with.
//
// The derivation sits AFTER the disk is materialized and re-fetched from
// state, and that placement is the whole of it: a new disk arrives as a
// pending struct carrying the name from -name, the review's entry 1 renames
// it on vmConfig only, and the struct that comes back out of state after
// creation is the one with the final name. Deriving any earlier reads the
// pre-rename name, and the VM boots on an address belonging to a disk that no
// longer exists -- which nothing else in this file can see, because every
// other MAC test drives a disk that is never renamed.
func TestStartDerivesTheMACFromTheRenamedDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test asserts the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it records anything, and this test's isolation from host binaries denies it one", runtime.GOOS)
	}
	const finalName = "renamed-disk"
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	onlyFakeQemuImgOnPath(t)
	stubLinuxARM64Firmware(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })

	// A disk name that does not exist yet, so this is a new disk and entry 1
	// is editable. The script renames it, leaves the menu, and presses Enter
	// to start. user mode, because it is the one that reaches the recording
	// step without preparing anything on the host.
	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "original-disk", "-iso", localISO(t), "-network", "user"},
		scriptedInput("1\n"+finalName+"\n\n\n"), &stdout, &stderr, "test")
	if err == nil || !strings.Contains(err.Error(), "start qemu") {
		t.Fatalf("start returned %v, want it to have recorded the VM and then failed to launch it; stdout:\n%s", err, stdout.String())
	}

	st := loadStoredState(t)
	if state.FindDiskByName(st, "original-disk") != nil {
		t.Fatalf("the rename did not take, so the ordering is not being exercised:\n%+v", st.Disks)
	}
	disk := state.FindDiskByName(st, finalName)
	if disk == nil {
		t.Fatalf("no disk named %q in state:\n%+v", finalName, st.Disks)
	}
	want := vm.MACForDisk(finalName)
	if disk.MAC != want {
		t.Errorf("state records MAC %q for %s, want %q (%q derives to %q)",
			disk.MAC, finalName, want, "original-disk", vm.MACForDisk("original-disk"))
	}
	wantArg := "virtio-net-pci,netdev=net0,mac=" + want
	if !slices.Contains(soleVM(t, st).QemuArgs, wantArg) {
		t.Errorf("the recorded qemu command line has no %q:\n%q", wantArg, soleVM(t, st).QemuArgs)
	}
}

// onlyFakeQemuImgOnPath puts a single executable on PATH: a qemu-img that
// creates the empty file it is asked to create.
//
// isolateFromHostBinaries is the right tool almost everywhere in this file,
// but a test about a NEW disk cannot use it: runStart removes any stale image
// and calls vm.EnsureDisk, which shells out to qemu-img, and a failure there
// ends the run several steps before the address is derived or anything is
// recorded. A double is what makes the rest of the run reachable, and it is
// still the same isolation -- the directory holds nothing else, so qemu-img
// is the only binary any lookup finds, and no real disk image is written.
//
// /bin/sh rather than a compiled helper: the interpreter is named absolutely
// in the shebang, so the kernel finds it whatever PATH says, and the script
// body is one redirection, which is a shell builtin and needs no PATH either.
func onlyFakeQemuImgOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// `qemu-img create -f qcow2 <path> <size>`, so $4 is the image path.
	script := "#!/bin/sh\n: > \"$4\"\n"
	if err := os.WriteFile(filepath.Join(dir, "qemu-img"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qemu-img: %v", err)
	}
	t.Setenv("PATH", dir)
}

// A derived address is not the same address for two disks, which is the reason
// the field exists at all. vm.MACForDisk owns the derivation and is tested
// there; this is about the wiring handing it the disk's own name.
func TestStartDerivesADifferentMACForADifferentDisk(t *testing.T) {
	if vm.MACForDisk("kairos-disk0") == vm.MACForDisk("kairos-disk1") {
		t.Fatal("two disk names derive the same address, so per-disk addressing buys nothing")
	}
}

// The firmware search has to accept the first candidate that is a real file,
// and ignore a directory that happens to carry the same name.
//
// firstReal sits before present and is itself a real file, so the two real
// candidates disagree about which one a last-wins bug would return. Without
// it {missing, asDir, present} carries exactly one real file, and a mutation
// that assigned inside the loop and returned after it -- last real file wins,
// not first -- would still return present and this test would not notice.
func TestFirstExistingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.fd")
	asDir := filepath.Join(dir, "as-dir.fd")
	firstReal := filepath.Join(dir, "first-real.fd")
	present := filepath.Join(dir, "present.fd")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstReal, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(present, []byte("firmware"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := firstExistingFile([]string{missing, asDir, firstReal, present}); got != firstReal {
		t.Errorf("got %q, want %q (the first real file, not the last)", got, firstReal)
	}
	if got := firstExistingFile([]string{missing}); got != "" {
		t.Errorf("got %q, want an empty path", got)
	}
}

// An arm64 guest boots nothing without EDK2, so the failure has to say which
// package supplies it (kairos-io/kairos#4858).
//
// This test does not stub linuxARM64Firmware: it is exercising the real list,
// so a host that genuinely has firmware installed has nothing wrong to
// report and is skipped rather than failed.
//
// Every candidate path and every package name is required, not just one of
// each: a message asserted against with "contains at least one of" passes
// even after the list is truncated to a single bogus path, or the package
// parenthetical is shrunk to one name -- both silently drop information a
// user needs to fix their host, and neither used to fail this test.
//
// The wanted paths are literals here, not linuxARM64Firmware itself. The
// error text is built from strings.Join(linuxARM64Firmware, ", "), so
// iterating that same slice to check the message is near-tautological: it
// can only fail if the %s verb is dropped, and a truncated or corrupted
// linuxARM64Firmware would still "pass" against itself. Literals catch that
// -- if the production list stops naming one of these paths, this fails
// regardless of what the list was mutated to.
func TestLinuxARM64FirmwarePathNamesTheCandidates(t *testing.T) {
	if firstExistingFile(linuxARM64Firmware) != "" {
		t.Skip("this host has arm64 firmware installed")
	}
	_, err := linuxARM64FirmwarePath()
	if err == nil {
		t.Fatal("expected an error when no firmware is installed")
	}
	wantPaths := []string{
		"/usr/share/AAVMF/QEMU_EFI.fd",
		"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
		"/usr/share/edk2/aarch64/QEMU_EFI.fd",
		"/usr/share/qemu/qemu-uefi-aarch64.bin",
		"/usr/share/qemu/edk2-aarch64-code.fd",
		"/usr/share/AAVMF/AAVMF_CODE.fd",
	}
	for _, want := range wantPaths {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name candidate path %q", err, want)
		}
	}
	// Anchored on the remedy clause alone, not the whole message: the message
	// also contains strings.Join(linuxARM64Firmware, ", "), which already
	// spells "aavmf" (from AAVMF_CODE.fd) and satisfies three of these five
	// names by way of a PATH, not a package name -- so asserting against the
	// full error lets the remedy clause be deleted outright and still pass.
	// Splitting on " -- " isolates the tail that actually names packages; a
	// message with no such split -- the remedy clause deleted outright --
	// leaves remedy empty, which fails every one of the five checks below
	// rather than short-circuiting on the first.
	remedy := ""
	if _, tail, ok := strings.Cut(err.Error(), " -- "); ok {
		remedy = tail
	}
	wantPackages := []string{"qemu-efi-aarch64", "edk2-aarch64", "aavmf", "qemu-uefi-aarch64", "qemu-system-aarch64"}
	for _, want := range wantPackages {
		if !strings.Contains(remedy, want) {
			t.Errorf("remedy clause %q should name package %q", remedy, want)
		}
	}
}

// firmwarePathFor is the seam that makes runStart's linux/arm64 firmware
// branch reachable from an amd64 test host: it takes goos and goarch as
// plain arguments instead of reading runtime.GOOS/runtime.GOARCH itself,
// the same shape buildLinuxFor uses in internal/vm for the same reason.
//
// This is the regression test for the failure mode that shipped once
// already: runStart used to switch on runtime.GOOS/runtime.GOARCH inline, so
// the linux/arm64 case it added was invisible to the amd64 CI leg that runs
// every test in this package -- deleting that case left `go test ./...`
// fully green. Table-testing firmwarePathFor directly is what makes that
// deletion fail here regardless of which host runs the suite.
//
// No ("darwin", ...) row: macOSFirmwarePath self-guards on the real
// runtime.GOOS rather than on the goos argument, so a darwin row run on a
// non-darwin host returns "", nil without exercising anything -- that path
// is covered by app_darwin_test.go, on the one leg that can run it for real.
//
// Before any of that: firmwareHostPlatform's default body -- `return
// runtime.GOOS, runtime.GOARCH` -- is executed by no test in this file.
// Every test that touches the seam, including the table below, replaces the
// whole var with a stub, so the real closure never runs under test. Mutate
// its body to `return runtime.GOARCH, runtime.GOOS` and go test ./... stays
// green throughout, because nothing here calls the unstubbed var. Left
// undetected, that mutation is not hypothetical: on a real linux/arm64 host
// it produces firmwarePathFor("arm64", "linux"), which falls into the
// default: arm and returns "", nil -- the exact regression this branch
// exists to fix, reproduced one level down inside the seam built to prevent
// it. This is the guard for that, and it runs on every leg without needing
// an arm64 host: it calls the real, unstubbed firmwareHostPlatform and
// checks it against runtime.GOOS/runtime.GOARCH directly.
//
// The sibling seams declared alongside it -- requireNetworkPrivilege,
// prepareLinuxBridge, prepareLinuxShared, buildQEMUCommand -- are bare
// function values with no body of their own (each is just `= vm.SomeFunc`),
// so there is nothing in them for a mutation to hide in, and none of them
// needs a guard like this one.
func TestFirmwarePathFor(t *testing.T) {
	if goos, goarch := firmwareHostPlatform(); goos != runtime.GOOS || goarch != runtime.GOARCH {
		t.Errorf("firmwareHostPlatform() = (%q, %q), want the real host (%q, %q)", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	stubbedFirmware := stubLinuxARM64Firmware(t)

	cases := []struct {
		name     string
		goos     string
		goarch   string
		wantPath string
	}{
		{name: "linux/arm64 resolves the stubbed firmware", goos: "linux", goarch: "arm64", wantPath: stubbedFirmware},
		{name: "linux/amd64 needs no firmware", goos: "linux", goarch: "amd64", wantPath: ""},
		{name: "an OS this tool builds no firmware lookup for", goos: "windows", goarch: "amd64", wantPath: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := firmwarePathFor(tc.goos, tc.goarch)
			if err != nil {
				t.Fatalf("firmwarePathFor(%q, %q) error = %v, want nil", tc.goos, tc.goarch, err)
			}
			if path != tc.wantPath {
				t.Errorf("firmwarePathFor(%q, %q) = %q, want %q", tc.goos, tc.goarch, path, tc.wantPath)
			}
		})
	}
}

// TestFirmwarePathFor above proves firmwarePathFor itself picks the right
// branch. It does not prove runStart calls it, calls it with the arguments in
// the right order, or still calls it at all rather than the pre-fix
// macOSFirmwarePath() -- and internal/vm's buildLinuxFor("amd64", ...) drops
// cfg.BiosPath before it becomes a QEMU argument, so no assertion against the
// built command line can tell those three failures apart from a correct run,
// on the one CI leg (amd64) that runs this whole package.
//
// This test closes that gap by stubbing every seam the wiring goes through:
// firmwareHostPlatform reports a linux/arm64 host that this test host is
// not, linuxARM64Firmware is stubbed to a temp file only that reported host
// would resolve, and buildQEMUCommand captures the vm.StartConfig runStart
// actually built instead of one this test constructed by hand. If runStart
// stopped calling firmwarePathFor (biosPath := ""), or called it with goos
// and goarch swapped, or reverted to calling macOSFirmwarePath() directly --
// the literal code this whole branch exists to replace, and which happens to
// return the right answer on darwin and an empty one on every other GOOS --
// this fails on every leg, macOS included, because firmwareHostPlatform is
// what says which branch to expect here regardless of the real host.
func TestStartResolvesLinuxARM64FirmwareForTheHostFirmwareHostPlatformReports(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		// The comment above says this fails on every leg, macOS included --
		// true of the failure it is guarding against, but not of an Intel
		// Mac: there, vm.BuildQEMUCommand routes on the real (unstubbed)
		// runtime.GOOS to buildMacOS, whose first statement refuses on
		// anything but arm64, before the launch that PATH isolation is what
		// is meant to make fail. See TestStartLaunchesVmnetSharedUnderSudo
		// in app_darwin_test.go for the same guard on the same mechanism.
		t.Skipf("macOS support is Apple Silicon only; on darwin/%s buildMacOS refuses before the launch is reached", runtime.GOARCH)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubbedFirmware := stubLinuxARM64Firmware(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })

	savedPlatform := firmwareHostPlatform
	t.Cleanup(func() { firmwareHostPlatform = savedPlatform })
	firmwareHostPlatform = func() (string, string) { return "linux", "arm64" }

	var captured vm.StartConfig
	savedBuild := buildQEMUCommand
	t.Cleanup(func() { buildQEMUCommand = savedBuild })
	buildQEMUCommand = func(cfg vm.StartConfig) (string, []string, error) {
		captured = cfg
		return savedBuild(cfg)
	}

	// user mode needs no uplink, no bridge/tap prepare and no sudo consent,
	// so nothing about networking is in the way of the one thing under test.
	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "user", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test")
	// The state is written and buildQEMUCommand has already run by
	// "[2/3] Recording VM state", one step before the launch that PATH
	// isolation makes fail -- the same point TestStartRecordsTheUplinkThePrepareUsed
	// stops at, and for the same reason.
	if err == nil || !strings.Contains(err.Error(), "start qemu") {
		t.Fatalf("start returned %v, want it to have recorded the VM and then failed to launch it; stdout:\n%s", err, stdout.String())
	}

	if captured.BiosPath != stubbedFirmware {
		t.Errorf("runStart built vm.StartConfig with BiosPath %q, want %q -- the firmware firmwarePathFor(firmwareHostPlatform()) resolves for the host firmwareHostPlatform reports",
			captured.BiosPath, stubbedFirmware)
	}
}

// teardownStartedLine is what reset and cleanup print when they begin taking
// the network apart, and it names no mode on purpose.
//
// It used to name st.Network.Mode. That field is written by EVERY start,
// including `-network user`, while the branch that prints this line gates on
// st.Network.CreatedByKairosLab -- which only the two modes that build a
// bridge ever set. A bridged run followed by `start -network user` therefore
// had `reset` announce "Cleaning up user network..." over the bridge and tap
// the bridged run had left, and user mode never builds either. Nothing in
// state records which mode PREPARED the network, so the mode is not a sound
// source for this sentence and no amount of validating it makes it one.
//
// Saying only that the network is kairos-lab's own is true of shared and
// bridged alike, and it is the thing the user needs from this line: the plan
// printed above it already names the bridge and the tap.
const teardownStartedLine = "Cleaning up the network kairos-lab created..."

// The message is the one the user reads while their network is being taken
// apart, and it may not describe it by a mode state cannot vouch for.
//
// "user" is the row that matters most: a stored mode of user with the
// teardown branch taken is exactly the state a bridged run followed by a user
// run leaves behind, and it used to produce a sentence about a "user network"
// that has never existed. The two real modes are here because the old wording
// was right for one of them and wrong for the other, and the injected one
// because a mode read out of state.json was printed into a terminal at all.
//
// The malformed bridge name makes the cleanup refuse before it probes or
// touches anything, exactly as the tests above it do, so what is asserted is
// the line printed on the way there and nothing on the host is involved.
func TestTeardownMessagesNameNoMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the network teardown only runs on linux")
	}
	modes := []struct {
		name string
		mode string
	}{
		{"shared", "shared"},
		{"bridged", "bridged"},
		{"user, which builds no network at all", "user"},
		{"nothing recorded", ""},
		{"a stored mode no version of this CLI accepts", injectedNetworkMode},
	}
	for _, verb := range []string{"reset", "cleanup"} {
		for _, tc := range modes {
			t.Run(verb+"/"+tc.name, func(t *testing.T) {
				t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
				t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
				seedInjectedState(t, func(st *state.State) {
					withNetworkNames(injectedBridgeName, "")(st)
					st.Network.Mode = tc.mode
				})

				var stdout, stderr bytes.Buffer
				// The refusal the malformed name produces is the business of
				// TestResetReportsAFailedNetworkCleanup and its cleanup
				// sibling; this one is about the line above it.
				_ = Run([]string{verb, "-yes"}, strings.NewReader(""), &stdout, &stderr, "test")
				out := stdout.String()
				if !strings.Contains(out, teardownStartedLine) {
					t.Errorf("%s does not announce %q; got:\n%s", verb, teardownStartedLine, out)
				}
				// No mode, by any spelling. The stored one is the only value
				// that could put a mode in this line, so each of these is a
				// sentence the tool would be making up.
				for _, forbidden := range []string{
					"Cleaning up shared network",
					"Cleaning up bridged network",
					"Cleaning up user network",
					"Cleaning up network...",
				} {
					if strings.Contains(out, forbidden) {
						t.Errorf("%s described the teardown as %q, which state cannot vouch for:\n%s", verb, forbidden, out)
					}
				}
				assertPlanIsInert(t, out)
			})
		}
	}
}

// seedStartableState writes a state.json that `start` will run against: setup
// complete, and one existing disk so the run resolves a disk without reaching
// the ISO resolver or creating anything.
func seedStartableState(t *testing.T, diskName string) {
	t.Helper()
	seedStartableStateWithDisks(t, diskName)
}

// seedStartableStateWithDisks is seedStartableState for more than one disk at
// once, so a test can start two VMs in the same config dir without one
// start's own seeding wiping out the disk the other one needs.
func seedStartableStateWithDisks(t *testing.T, diskNames ...string) {
	t.Helper()
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	st := state.NewState(store)
	st.Setup.CompletedAt = state.NowRFC3339()
	st.Setup.DependencyCheckPassed = true
	for _, diskName := range diskNames {
		st.Disks = append(st.Disks, state.Disk{
			Name:      diskName,
			Path:      filepath.Join(store.CacheDir, "vm", diskName+".qcow2"),
			Size:      "60G",
			CreatedAt: state.NowRFC3339(),
		})
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// reviewableConfig is the smallest config reviewVMConfig can render: it calls
// freeSpaceGB on the disk path's directory (which must exist for df to answer,
// though a 0 answer is handled), systemRAMGB, and parseSizeGB on the size
// string. Nothing here is written to, and no disk image is created.
func reviewableConfig(t *testing.T) *vmStartConfig {
	t.Helper()
	return &vmStartConfig{
		DiskName:     "kairos-disk0",
		DiskPath:     filepath.Join(t.TempDir(), "kairos-disk0.qcow2"),
		DiskSize:     "60G",
		MemoryGB:     4,
		CPUs:         2,
		NetworkMode:  "bridged",
		Display:      "window",
		DownloadsDir: t.TempDir(),
		TakenNames:   map[string]struct{}{},
	}
}

// scriptedInput hands back one line per Read, the way a terminal in canonical
// mode does.
//
// It used to be the ONLY reader that could drive reviewVMConfig: the menu
// rebuilt a bufio.Reader every iteration and prompt() built another for every
// sub-prompt, so a reader that answered the first Read with the whole script
// lost every line but the first, and the reviewer returned "no input". That
// was kairos-io/kairos#5057, and a plain strings.Reader drives these paths
// now; the specs named ...FromPipedStdin are the ones that hold it fixed.
//
// This helper stays because a tty is still worth covering in its own right:
// it is the shape real interactive use has, and it is what the specs below
// were written against.
func scriptedInput(script string) io.Reader {
	lines := strings.SplitAfter(script, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			out = append(out, line)
		}
	}
	return &lineReader{lines: out}
}

type lineReader struct{ lines []string }

func (r *lineReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.lines[0])
	if n < len(r.lines[0]) {
		r.lines[0] = r.lines[0][n:]
		return n, nil
	}
	r.lines = r.lines[1:]
	return n, nil
}

// --- the address the guest ends up on --------------------------------------

// assertBlockColumns fails when a row of one of the address blocks does not
// start its value at column 11.
//
// The alignment is the point of the block: three or four rows with their
// labels padded to a common column, so a user reads the values down one edge
// rather than hunting for them. A continuation row carries no label and has
// to line up under the values for the same reason.
func assertBlockColumns(t *testing.T, block string) {
	t.Helper()
	const valueColumn = 10 // zero-based, so column 11 to a human
	rows := strings.Split(block, "\n")
	if len(rows) < 2 {
		t.Fatalf("a block with no rows under its heading:\n%q", block)
	}
	for _, row := range rows[1:] {
		if !strings.HasPrefix(row, "  ") {
			t.Errorf("row %q is not indented, so it does not read as part of the block", row)
			continue
		}
		at := len(row) - len(strings.TrimLeft(row, " "))
		if colon := strings.Index(row, ":"); colon >= 0 && colon < valueColumn {
			rest := row[colon+1:]
			at = colon + 1 + len(rest) - len(strings.TrimLeft(rest, " "))
		}
		if at != valueColumn {
			t.Errorf("row %q starts its value at column %d, want column %d", row, at+1, valueColumn+1)
		}
	}
}

// The success block, character for character. The first three lines are the
// ones the issue specifies and may not drift by a space; the fourth is the
// source, which vm.IPResult's own doc asks the caller to print because an
// address from the ARP cache and the same address from our own DHCP server's
// lease file deserve different amounts of trust.
func TestVMUpBlockIsTheBlockTheIssueSpecifies(t *testing.T) {
	tests := []struct {
		name string
		res  vm.IPResult
		want string
	}{
		{
			name: "shared, from the DHCP server this tool started",
			res:  vm.IPResult{IP: "192.168.64.12", Source: vm.IPSourceDHCPLease},
			want: "VM is up.\n" +
				"  WebUI:  http://192.168.64.12:8080\n" +
				"  SSH:    ssh kairos@192.168.64.12\n" +
				"  Source: dhcp-lease",
		},
		{
			name: "bridged, from the host's neighbour table",
			res:  vm.IPResult{IP: "10.0.1.42", Source: vm.IPSourceARP},
			want: "VM is up.\n" +
				"  WebUI:  http://10.0.1.42:8080\n" +
				"  SSH:    ssh kairos@10.0.1.42\n" +
				"  Source: arp",
		},
		{
			name: "from the guest's own agent",
			res:  vm.IPResult{IP: "192.168.64.13", Source: vm.IPSourceGuestAgent},
			want: "VM is up.\n" +
				"  WebUI:  http://192.168.64.13:8080\n" +
				"  SSH:    ssh kairos@192.168.64.13\n" +
				"  Source: qemu-guest-agent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vmUpBlock(tt.res)
			if got != tt.want {
				t.Errorf("vmUpBlock(%+v) =\n%q\nwant\n%q", tt.res, got, tt.want)
			}
			assertBlockColumns(t, got)
		})
	}
}

// The block is built out of values this package did not choose, so it goes
// through the same guard the plans do. A terminal may not act on it whatever
// internal/vm hands back.
func TestVMUpBlockIsInertForAnAddressATerminalWouldActOn(t *testing.T) {
	got := vmUpBlock(vm.IPResult{
		IP:     "192.168.64.12\x1b[2K\r10.0.0.1",
		Source: "dhcp-lease\nVM is up.\x1b[2K\r",
	})
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("the block carries a raw escape byte:\n%q", got)
	}
	if strings.Contains(got, "\nVM is up.") {
		t.Errorf("the block carries a forged heading:\n%q", got)
	}
}

// The block for an address that was found and is no use.
//
// A guest whose DHCP request went unanswered assigns itself a 169.254
// address, the host's ARP cache picks it up like any other, and vm's
// usableIPv4 passes link-local through on purpose -- so this arrives on the
// success path, and the issue's requirement ("never exit silently leaving
// the user with only a link-local address") is about exactly this output.
func TestVMLinkLocalBlockSaysWhatASelfAssignedAddressMeans(t *testing.T) {
	tests := []struct {
		name string
		res  vm.IPResult
		f    ipPollFacts
		want string
	}{
		{
			name: "shared, picked out of the host's neighbour table",
			res:  vm.IPResult{IP: "169.254.11.9", Source: vm.IPSourceARP},
			f: ipPollFacts{
				Mode: "shared", MAC: "52:54:00:ab:cd:ef", Bridge: "kairoslab0",
				GOOS: "linux", Timeout: vm.DefaultIPPollTimeout,
			},
			want: "The VM is up, but the address found for it is link-local.\n" +
				"  WebUI:  http://169.254.11.9:8080\n" +
				"  SSH:    ssh kairos@169.254.11.9\n" +
				"  Source: arp\n" +
				"  Cause:  169.254.0.0/16 is what a guest assigns itself when no DHCP\n" +
				"          server answers it. It is not routed, so the two URLs above\n" +
				"          will not reach the VM.\n" +
				"  Check:  the bridge kairoslab0 is up and the DHCP server behind it is running.",
		},
		{
			name: "bridged, where the LAN's DHCP server is the one that stayed quiet",
			res:  vm.IPResult{IP: "169.254.200.1", Source: vm.IPSourceGuestAgent},
			f: ipPollFacts{
				Mode: "bridged", MAC: "52:54:00:ab:cd:ef", Uplink: "eth0",
				GOOS: "linux", Timeout: vm.DefaultIPPollTimeout,
			},
			want: "The VM is up, but the address found for it is link-local.\n" +
				"  WebUI:  http://169.254.200.1:8080\n" +
				"  SSH:    ssh kairos@169.254.200.1\n" +
				"  Source: qemu-guest-agent\n" +
				"  Cause:  169.254.0.0/16 is what a guest assigns itself when no DHCP\n" +
				"          server answers it. It is not routed, so the two URLs above\n" +
				"          will not reach the VM.\n" +
				"  Check:  eth0 has a link and a DHCP server on that network answered.",
		},
		{
			name: "bridged on macOS keeps the ARP caveat the timeout notice gives",
			res:  vm.IPResult{IP: "169.254.11.9", Source: vm.IPSourceARP},
			f: ipPollFacts{
				Mode: "bridged", MAC: "52:54:00:ab:cd:ef", Uplink: "en0",
				GOOS: "darwin", Timeout: vm.DefaultIPPollTimeout,
			},
			want: "The VM is up, but the address found for it is link-local.\n" +
				"  WebUI:  http://169.254.11.9:8080\n" +
				"  SSH:    ssh kairos@169.254.11.9\n" +
				"  Source: arp\n" +
				"  Cause:  169.254.0.0/16 is what a guest assigns itself when no DHCP\n" +
				"          server answers it. It is not routed, so the two URLs above\n" +
				"          will not reach the VM.\n" +
				"  Check:  en0 has a link and a DHCP server on that network answered.\n" +
				"          On macOS this is answered from the host ARP cache, which\n" +
				"          holds no entry until this host and the guest have\n" +
				"          exchanged frames.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vmLinkLocalBlock(tt.res, tt.f)
			if got != tt.want {
				t.Errorf("vmLinkLocalBlock(%+v, %+v) =\n%q\nwant\n%q", tt.res, tt.f, got, tt.want)
			}
			assertBlockColumns(t, got)
			// The three things the block exists to say, asserted apart from
			// the exact bytes: a rewording is free, dropping one is not.
			// The address is one of them -- it is still what the host found
			// for this guest, and withholding it would leave a user who
			// wants to try it anyway with nothing.
			for _, want := range []string{tt.res.IP, "self", "DHCP", "not reach", "Check:"} {
				if !strings.Contains(got, want) {
					t.Errorf("the block does not carry %q:\n%s", want, got)
				}
			}
			// And the one thing it may never read as. "VM is up." on its own
			// line is the issue's success heading; this block has to be
			// distinguishable from it at a glance.
			if strings.HasPrefix(got, "VM is up.") || strings.Contains(got, "\nVM is up.") {
				t.Errorf("a link-local address is presented as plain success:\n%s", got)
			}
		})
	}
}

// The same guard the success block gets: the values come from internal/vm,
// which read them out of a lease file or a host command's output.
func TestVMLinkLocalBlockIsInertForValuesATerminalWouldActOn(t *testing.T) {
	got := vmLinkLocalBlock(
		vm.IPResult{IP: "169.254.11.9", Source: "arp\nVM is up.\x1b[2K\r"},
		ipPollFacts{Mode: "shared", Bridge: injectedBridgeName, GOOS: "linux"},
	)
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("the block carries a raw escape byte:\n%q", got)
	}
	if strings.Contains(got, "\nVM is up.") {
		t.Errorf("the block carries a forged success heading:\n%q", got)
	}
}

// isLinkLocalIPv4 decides which of the two blocks a resolved address gets,
// and it also annotates the `status` row, where the value comes straight out
// of a 0644 state.json and need not be an address at all.
func TestIsLinkLocalIPv4(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"169.254.11.9", true},
		{"169.254.0.0", true},
		{"169.254.255.255", true},
		{"::ffff:169.254.11.9", true}, // the IPv4-mapped form usableIPv4 folds
		{"192.168.64.12", false},
		{"10.0.1.42", false},
		{"169.255.0.1", false}, // one octet past the range
		{"169.253.255.255", false},
		{"fe80::1", false}, // link-local, but not an IPv4 address
		{"", false},
		{"none", false},
		{"192.168.64.12\nvm running: true", false},
	}
	for _, tt := range tests {
		if got := isLinkLocalIPv4(tt.ip); got != tt.want {
			t.Errorf("isLinkLocalIPv4(%q) = %t, want %t", tt.ip, got, tt.want)
		}
	}
}

// user mode is told where the VM is instead of being polled for an address
// it can never have. The block is printed before QEMU is started, so it says
// where the VM will be reached and never that it is up.
func TestUserModeBlockNamesTheForwardedPortsAndTheLimit(t *testing.T) {
	want := "user mode: the guest sits behind QEMU's user-mode NAT.\n" +
		"  WebUI:  http://localhost:8080\n" +
		"  SSH:    ssh -p 2222 kairos@localhost\n" +
		"  Note:   those two forwarded ports are the only way in. The guest\n" +
		"          has no address on your network, so this mode supports a\n" +
		"          single VM and cannot form a cluster."
	got := userModeBlock()
	if got != want {
		t.Errorf("userModeBlock() =\n%q\nwant\n%q", got, want)
	}
	assertBlockColumns(t, got)
	if strings.Contains(got, "VM is up") {
		t.Errorf("the block claims a VM is up, and it is printed before QEMU is started:\n%s", got)
	}
}

// The two ports the block names are the two QEMU is told to forward. Nothing
// in internal/vm exports them -- both builders carry the hostfwd list as a
// literal -- so this is what keeps the sentence the user reads and the
// command line that is run from drifting apart.
func TestUserModeBlockNamesThePortsQEMUIsToldToForward(t *testing.T) {
	_, args, err := vm.BuildQEMUCommand(vm.StartConfig{
		DiskPath:      "/nope/kairos-disk0.qcow2",
		QGASocketPath: "/nope/qemu.sock",
		CPUs:          2,
		MemoryMB:      2048,
		NetworkMode:   "user",
		DisplayMode:   "serial",
		MACAddress:    vm.MACForDisk("kairos-disk0"),
		BiosPath:      "/nope/edk2-aarch64-code.fd",
	})
	if err != nil {
		t.Fatalf("BuildQEMUCommand: %v", err)
	}
	netdev := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "user,id=net0") {
			netdev = arg
		}
	}
	if netdev == "" {
		t.Fatalf("no user-mode netdev on the command line:\n%q", args)
	}
	for _, want := range []string{
		"hostfwd=tcp::" + userModeSSHPort + "-:22",
		"hostfwd=tcp::" + webUIPort + "-:8080",
	} {
		if !strings.Contains(netdev, want) {
			t.Errorf("the block names a port QEMU does not forward: %q is not in %q", want, netdev)
		}
	}
	block := userModeBlock()
	for _, want := range []string{"localhost:" + webUIPort, "-p " + userModeSSHPort} {
		if !strings.Contains(block, want) {
			t.Errorf("the block does not name %q:\n%s", want, block)
		}
	}
}

// The diagnostic a user gets instead of an address. It has to say what was
// asked (the mode and the MAC, which is what a lease file or an ARP table is
// searched by), how long it waited, what usually causes this, one thing to
// check for this mode, and where the recorded answer lives.
func TestVMIPTimeoutNoticeSaysWhyAndWhatToTry(t *testing.T) {
	const mac = "52:54:00:ab:cd:ef"
	tests := []struct {
		name string
		f    ipPollFacts
		want string
	}{
		{
			name: "shared names the bridge and its DHCP server",
			f: ipPollFacts{
				Mode: "shared", MAC: mac, Bridge: "kairoslab0",
				GOOS: "linux", Timeout: vm.DefaultIPPollTimeout,
			},
			want: "No address for the VM after 45s. This run has stopped looking.\n" +
				"  Mode:   shared\n" +
				"  MAC:    52:54:00:ab:cd:ef\n" +
				"  Cause:  the guest may still be booting, or it never got a lease.\n" +
				"  Check:  the bridge kairoslab0 is up and the DHCP server behind it is running.\n" +
				"  Then:   kairos-lab status, for what this run recorded.",
		},
		{
			name: "bridged names the uplink and the LAN's DHCP server",
			f: ipPollFacts{
				Mode: "bridged", MAC: mac, Uplink: "eth0",
				GOOS: "linux", Timeout: vm.DefaultIPPollTimeout,
			},
			want: "No address for the VM after 45s. This run has stopped looking.\n" +
				"  Mode:   bridged\n" +
				"  MAC:    52:54:00:ab:cd:ef\n" +
				"  Cause:  the guest may still be booting, or it never got a lease.\n" +
				"  Check:  eth0 has a link and a DHCP server on that network answered.\n" +
				"  Then:   kairos-lab status, for what this run recorded.",
		},
		{
			name: "bridged on macOS adds the ARP cache caveat",
			f: ipPollFacts{
				Mode: "bridged", MAC: mac, Uplink: "en0",
				GOOS: "darwin", Timeout: vm.DefaultIPPollTimeout,
			},
			want: "No address for the VM after 45s. This run has stopped looking.\n" +
				"  Mode:   bridged\n" +
				"  MAC:    52:54:00:ab:cd:ef\n" +
				"  Cause:  the guest may still be booting, or it never got a lease.\n" +
				"  Check:  en0 has a link and a DHCP server on that network answered.\n" +
				"          On macOS this is answered from the host ARP cache, which\n" +
				"          holds no entry until this host and the guest have\n" +
				"          exchanged frames.\n" +
				"  Then:   kairos-lab status, for what this run recorded.",
		},
		{
			name: "shared on macOS, where no bridge name is ever recorded",
			f: ipPollFacts{
				Mode: "shared", MAC: mac,
				GOOS: "darwin", Timeout: vm.DefaultIPPollTimeout,
			},
			want: "No address for the VM after 45s. This run has stopped looking.\n" +
				"  Mode:   shared\n" +
				"  MAC:    52:54:00:ab:cd:ef\n" +
				"  Cause:  the guest may still be booting, or it never got a lease.\n" +
				"  Check:  the NAT bridge is up and the DHCP server behind it is running.\n" +
				"  Then:   kairos-lab status, for what this run recorded.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vmIPTimeoutNotice(tt.f)
			if got != tt.want {
				t.Errorf("vmIPTimeoutNotice(%+v) =\n%q\nwant\n%q", tt.f, got, tt.want)
			}
			assertBlockColumns(t, got)
			// The elements the notice exists for, asserted separately from
			// the exact bytes: a rewording is free, dropping one is not.
			for _, want := range []string{tt.f.Mode, tt.f.MAC, "45s", "kairos-lab status", "Cause:", "Check:"} {
				if !strings.Contains(got, want) {
					t.Errorf("the notice does not carry %q:\n%s", want, got)
				}
			}
		})
	}
}

// The names in the notice come out of state.json like every other stored
// value this CLI prints.
func TestVMIPTimeoutNoticeIsInertForStoredNames(t *testing.T) {
	got := vmIPTimeoutNotice(ipPollFacts{
		Mode:    "shared",
		MAC:     "52:54:00:ab:cd:ef\n  Check:  nothing is wrong\x1b[2K\r",
		Bridge:  injectedBridgeName,
		GOOS:    "linux",
		Timeout: vm.DefaultIPPollTimeout,
	})
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("the notice carries a raw escape byte:\n%q", got)
	}
	if strings.Contains(got, "\n  Check:  nothing is wrong") {
		t.Errorf("the notice carries a forged row:\n%q", got)
	}
}

// stubIPPoll puts a scripted answer in place of the real address lookup.
//
// The real vm.IPLookup.Poll runs its three sources for real on every tick: a
// subprocess against the host's neighbour table and a read on a guest-agent
// socket, which runStart gives 45 seconds and a one-second interval. What it
// answers on the machine running this suite is whatever that machine's ARP
// cache holds -- and an entry against a 52:54:00 MAC really can be in there,
// since that is QEMU's own prefix -- so the poll is scripted here, and what
// the wiring does with each answer is what these tests are about.
func stubIPPoll(t *testing.T, answer func(ctx context.Context, lookup vm.IPLookup, timeout, interval time.Duration) (vm.IPResult, bool)) {
	t.Helper()
	saved := pollVMIP
	t.Cleanup(func() { pollVMIP = saved })
	pollVMIP = answer
}

// An address that was found is printed AND written to state.json, because the
// two channels fail differently: -serial mon:stdio is on all four display
// branches in internal/vm, so the printed block shares a terminal with the
// guest's boot console and can scroll past unread. `status` is the durable
// one.
func TestIPPollPrintsTheAddressAndRecordsItForStatus(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}

	found := vm.IPResult{IP: "192.168.64.12", Source: vm.IPSourceDHCPLease}
	stubIPPoll(t, func(context.Context, vm.IPLookup, time.Duration, time.Duration) (vm.IPResult, bool) {
		return found, true
	})

	// recordVMIP (M4) keys its Update on the VM's own name, so there has to
	// be a record of that name for it to find; seedStartableState only adds
	// the disk.
	if err := store.Update(func(st *state.State) error {
		state.UpsertVM(st, state.VM{Name: "kairos-disk0"})
		return nil
	}); err != nil {
		t.Fatalf("seed vm record: %v", err)
	}

	var stdout, stderr bytes.Buffer
	poll := vmIPPoll{
		Name:     "kairos-disk0",
		Lookup:   vm.IPLookup{MAC: vm.MACForDisk("kairos-disk0"), Mode: "shared", BridgeName: "kairoslab0"},
		Timeout:  time.Hour,
		Interval: time.Second,
		GOOS:     runtime.GOOS,
		Stdout:   &stdout,
		Stderr:   &stderr,
		Store:    store,
	}
	res, ok := poll.run(context.Background())
	if !ok || res != found {
		t.Fatalf("run() = (%+v, %t), want (%+v, true)", res, ok, found)
	}
	if got, want := stdout.String(), vmUpBlock(found)+"\n"; got != want {
		t.Errorf("printed\n%q\nwant\n%q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("a successful lookup warned about something:\n%s", stderr.String())
	}
	st := loadStoredState(t)
	if soleVM(t, st).IPAddress != found.IP {
		t.Errorf("state records the address %q, want %q -- `status` has nothing to show", soleVM(t, st).IPAddress, found.IP)
	}
	// The poller loads and saves the state rather than writing one of its
	// own, so everything the start recorded is still there.
	if state.FindDiskByName(st, "kairos-disk0") == nil {
		t.Errorf("recording the address dropped the rest of the state:\n%+v", st)
	}
}

// Which block a resolved address gets, in exact bytes, at the seam that
// chooses: vm.IPLookup.Poll answers a lease and a self-assigned address with
// the same (res, true), so the difference between a VM a user can reach and
// one they cannot is made here or nowhere.
//
// The first case is the issue's block, character for character, and it is
// asserted through the wiring rather than only against the renderer: these
// are the bytes a start really puts on the terminal.
func TestIPPollTellsALeaseFromASelfAssignedAddress(t *testing.T) {
	const usable = "VM is up.\n" +
		"  WebUI:  http://192.168.64.12:8080\n" +
		"  SSH:    ssh kairos@192.168.64.12\n" +
		"  Source: dhcp-lease\n"
	const selfAssigned = "The VM is up, but the address found for it is link-local.\n" +
		"  WebUI:  http://169.254.11.9:8080\n" +
		"  SSH:    ssh kairos@169.254.11.9\n" +
		"  Source: arp\n" +
		"  Cause:  169.254.0.0/16 is what a guest assigns itself when no DHCP\n" +
		"          server answers it. It is not routed, so the two URLs above\n" +
		"          will not reach the VM.\n" +
		"  Check:  the bridge kairoslab0 is up and the DHCP server behind it is running.\n"

	tests := []struct {
		name string
		res  vm.IPResult
		want string
	}{
		{
			name: "an address from the DHCP server this tool started",
			res:  vm.IPResult{IP: "192.168.64.12", Source: vm.IPSourceDHCPLease},
			want: usable,
		},
		{
			name: "an address the guest gave itself when nothing answered",
			res:  vm.IPResult{IP: "169.254.11.9", Source: vm.IPSourceARP},
			want: selfAssigned,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			isolateFromHostBinaries(t)
			seedStartableState(t, "kairos-disk0")
			store, err := state.DefaultStore()
			if err != nil {
				t.Fatalf("DefaultStore: %v", err)
			}
			stubIPPoll(t, func(context.Context, vm.IPLookup, time.Duration, time.Duration) (vm.IPResult, bool) {
				return tt.res, true
			})
			if err := store.Update(func(st *state.State) error {
				state.UpsertVM(st, state.VM{Name: "kairos-disk0"})
				return nil
			}); err != nil {
				t.Fatalf("seed vm record: %v", err)
			}

			var stdout, stderr bytes.Buffer
			poll := vmIPPoll{
				Name:     "kairos-disk0",
				Lookup:   vm.IPLookup{MAC: vm.MACForDisk("kairos-disk0"), Mode: "shared", BridgeName: "kairoslab0"},
				Timeout:  time.Hour,
				Interval: time.Second,
				// Pinned rather than runtime.GOOS: shared's check line does
				// not branch on the platform today, and pinning it is what
				// keeps the bytes above the bytes on both CI legs if it ever
				// does.
				GOOS:   "linux",
				Stdout: &stdout,
				Stderr: &stderr,
				Store:  store,
			}
			res, ok := poll.run(context.Background())
			if !ok || res != tt.res {
				t.Fatalf("run() = (%+v, %t), want (%+v, true)", res, ok, tt.res)
			}
			if got := stdout.String(); got != tt.want {
				t.Errorf("printed\n%q\nwant\n%q", got, tt.want)
			}
			if stderr.Len() != 0 {
				t.Errorf("a resolved address warned about something:\n%s", stderr.String())
			}
			// Both are recorded. A self-assigned address is still what the
			// host found for this guest, and `status` is where a user reads
			// back what a start resolved; the row qualifies it there.
			if got := soleVM(t, loadStoredState(t)).IPAddress; got != tt.res.IP {
				t.Errorf("state records the address %q, want %q", got, tt.res.IP)
			}
		})
	}
}

// The two ways a poll ends with no address mean opposite things to the user,
// and vm.IPLookup.Poll answers both with the same false: a budget that ran
// out is a network worth looking at, a cancelled poll is a VM its owner quit.
// Telling the second one their network is broken is the failure this pins.
func TestIPPollDiagnosesATimeoutAndSaysNothingAboutAQuitVM(t *testing.T) {
	const budget = 10 * time.Millisecond
	lookup := vm.IPLookup{MAC: "52:54:00:ab:cd:ef", Mode: "shared", BridgeName: "kairoslab0"}

	tests := []struct {
		name    string
		timeout time.Duration
		poll    func(ctx context.Context, cancel context.CancelFunc) (vm.IPResult, bool)
		want    string
	}{
		{
			name:    "the budget ran out, which is worth a diagnostic",
			timeout: budget,
			poll: func(context.Context, context.CancelFunc) (vm.IPResult, bool) {
				// time.Sleep never returns early, so the whole budget has
				// certainly been spent by the time this answers.
				time.Sleep(budget + 2*time.Millisecond)
				return vm.IPResult{}, false
			},
			want: vmIPTimeoutNotice(ipPollFacts{
				Mode: "shared", MAC: "52:54:00:ab:cd:ef", Bridge: "kairoslab0",
				GOOS: runtime.GOOS, Timeout: budget,
			}) + "\n",
		},
		{
			name:    "the VM exited, which is not the network's fault",
			timeout: budget,
			poll: func(_ context.Context, cancel context.CancelFunc) (vm.IPResult, bool) {
				// The budget elapses here too, so what keeps this quiet can
				// only be the cancellation: this is runStart's QEMU-exited
				// path, where the poll is cancelled and then returns.
				cancel()
				time.Sleep(budget + 2*time.Millisecond)
				return vm.IPResult{}, false
			},
			want: "",
		},
		{
			name:    "the lookup refused at once, having waited for nothing",
			timeout: time.Hour,
			poll: func(context.Context, context.CancelFunc) (vm.IPResult, bool) {
				// vm.IPLookup.Poll answers an unusable MAC like this,
				// immediately. A notice here would say the tool waited an
				// hour for an address, which it did not.
				return vm.IPResult{}, false
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stubIPPoll(t, func(ctx context.Context, _ vm.IPLookup, _, _ time.Duration) (vm.IPResult, bool) {
				return tt.poll(ctx, cancel)
			})

			var stdout, stderr bytes.Buffer
			poll := vmIPPoll{
				Lookup:   lookup,
				Timeout:  tt.timeout,
				Interval: time.Millisecond,
				GOOS:     runtime.GOOS,
				Stdout:   &stdout,
				Stderr:   &stderr,
				Store:    nil, // never reached: nothing is recorded without an address
			}
			res, ok := poll.run(ctx)
			if ok {
				t.Fatalf("run() answered %+v, want no address", res)
			}
			if got := stdout.String(); got != tt.want {
				t.Errorf("printed\n%q\nwant\n%q", got, tt.want)
			}
			if stderr.Len() != 0 {
				t.Errorf("wrote to stderr:\n%s", stderr.String())
			}
		})
	}
}

// A start in user mode says where the VM is reached, starts no address poll,
// and leaves no stale address behind for `status` to report as this run's.
//
// The address of a previous run is not this run's: the lease can change, and
// until something resolves one there is nothing true to show.
//
// The VM is really launched here -- a fake qemu, but one that starts and
// exits -- and that is what makes the middle claim testable. Everything about
// the address in runStart sits AFTER command.Start(), so a version of this
// test that isolated PATH into nothing never reached the branch that decides
// whether to poll, and the scripted poll below could not have fired whatever
// that branch did.
//
// What the guard is for: a user-mode poll can only be answered by the guest
// agent, which reports the address the guest sees behind SLIRP -- 10.0.2.15,
// which nothing on this host can connect to. Printing it as "VM is up" would
// contradict the user-mode block printed seconds earlier, and recording it
// would leave `status` naming it as this VM's address. So the scripted poll
// answers rather than refuses, and what it answers is that address.
func TestStartInUserModeSaysWhereTheVMIsAndClearsTheOldAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test drives the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it launches anything, and the PATH this test sets holds nothing but a fake qemu", runtime.GOOS)
	}
	const diskName = "kairos-disk0"
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	fakeQEMUOnPath(t)
	stubLinuxARM64Firmware(t)
	seedStartableState(t, diskName)

	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	state.UpsertVM(st, state.VM{Name: diskName, IPAddress: "192.168.64.99"})
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	stubIPPoll(t, func(context.Context, vm.IPLookup, time.Duration, time.Duration) (vm.IPResult, bool) {
		t.Error("user mode started an address poll, which has no host source to answer it")
		return vm.IPResult{IP: "10.0.2.15", Source: vm.IPSourceGuestAgent}, true
	})

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"start", "-name", diskName, "-no-iso", "-network", "user", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
		t.Fatalf("start returned %v; stdout:\n%s", err, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "vm exited") {
		t.Fatalf("the run did not reach the end of the VM's life, so the branch that starts a poll was never passed:\n%s", out)
	}
	if !strings.Contains(out, userModeBlock()) {
		t.Errorf("the run does not say where a user-mode VM is reached; got:\n%s", out)
	}
	if strings.Contains(out, "VM is up.") {
		t.Errorf("user mode printed a resolved-address block, contradicting the user-mode block above it:\n%s", out)
	}
	if got := soleVM(t, loadStoredState(t)).IPAddress; got != "" {
		t.Errorf("state records %q as this VM's address, which `status` would report: the previous run's address was not cleared, or a poll this mode must not start recorded one", got)
	}
}

// --- status rows -----------------------------------------------------------

// runStatusOutput seeds a state, runs `status` against it and returns what
// the user would see. PATH is pointed at nothing so the dependency rows and
// the macOS link note answer the same way on every host.
func runStatusOutput(t *testing.T, mutate func(*state.State)) string {
	t.Helper()
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedInjectedState(t, mutate)

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"status"}, strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
		t.Fatalf("status: %v", err)
	}
	return stdout.String()
}

// What `status` reports about the network, per mode.
//
// The bridge and tap row used to be gated on bridged alone, so the default
// mode -- shared, which records both fields exactly as the bridged path does
// -- showed neither, and a user whose shared VM was unreachable had no way to
// name what to look at. The uplink row stays bridged-only, because shared
// clears that field on purpose: its bridge has the tap as its only port.
func TestStatusNetworkRowsForEachMode(t *testing.T) {
	withNetwork := func(mode string) func(*state.State) {
		return func(st *state.State) {
			st.Network.Mode = mode
			st.Network.BridgeName = "kairoslab0"
			st.Network.TapName = "kairoslab-tap0"
			st.Network.BridgeInterface = "eth0"
			withVMField(testVMName, func(v *state.VM) {
				v.IPAddress = "192.168.64.12"
				v.NetworkMode = mode
				v.TapName = "kairoslab-tap0"
			})(st)
		}
	}
	tests := []struct {
		name    string
		mode    string
		want    []string
		notWant []string
	}{
		{
			name: "shared shows the bridge it built",
			mode: "shared",
			want: []string{
				"  network mode: shared\n",
				"bridge resources: bridge=kairoslab0 taps=kairoslab-tap0\n",
				"  vm ip address: 192.168.64.12\n",
			},
			notWant: []string{"bridge iface:", "user mode forwards:"},
		},
		{
			name: "bridged shows the uplink as well",
			mode: "bridged",
			want: []string{
				"  network mode: bridged\n",
				"bridge iface: eth0",
				"bridge resources: bridge=kairoslab0 taps=kairoslab-tap0\n",
				"  vm ip address: 192.168.64.12\n",
			},
			notWant: []string{"user mode forwards:"},
		},
		{
			name: "user shows the forwarded ports instead of a bare address",
			mode: "user",
			want: []string{
				"  network mode: user\n",
				"  user mode forwards: ssh localhost:2222, http localhost:8080\n",
				"  vm ip address: 192.168.64.12\n",
			},
			notWant: []string{"bridge iface:", "bridge resources:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runStatusOutput(t, withNetwork(tt.mode))
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("status does not print %q:\n%s", want, out)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("status prints %q, which this mode does not have:\n%s", notWant, out)
				}
			}
		})
	}
}

// The bridge row is gated on there being something to put in it, and not on
// the mode.
//
// Only the LINUX prepares write BridgeName and TapName. On macOS both shared
// and bridged hand the bridging to QEMU's vmnet backend and record neither,
// so a mode gate printed "bridge resources: bridge=none tap=none" under every
// shared and every bridged run there -- a permanently empty row in the status
// of the default mode, on one of the two supported platforms.
func TestStatusOmitsTheBridgeRowWhenNoBridgeWasRecorded(t *testing.T) {
	for _, mode := range []string{"shared", "bridged"} {
		t.Run(mode, func(t *testing.T) {
			// A macOS-shaped state: the mode ran, and the two fields only
			// vm's Linux prepares ever write are empty.
			out := runStatusOutput(t, func(st *state.State) {
				st.Platform.OS = "darwin"
				st.Platform.Arch = "arm64"
				st.Network.Mode = mode
				st.Network.BridgeInterface = "en0"
				withVMField(testVMName, func(v *state.VM) {
					v.IPAddress = "192.168.64.12"
					v.NetworkMode = mode
				})(st)
			})
			if strings.Contains(out, "bridge resources:") {
				t.Errorf("status prints a bridge row this run has nothing for:\n%s", out)
			}
			// The rows this platform does have are untouched: the mode, the
			// address, and -- for bridged, which names an interface on macOS
			// too -- the uplink.
			want := []string{"  network mode: " + mode + "\n", "  vm ip address: 192.168.64.12\n"}
			if mode == "bridged" {
				want = append(want, "bridge iface: en0")
			}
			for _, row := range want {
				if !strings.Contains(out, row) {
					t.Errorf("status does not print %q:\n%s", row, out)
				}
			}
		})
	}

	// Half a pair recorded is the case a reader most needs to see, so the
	// gate is either field and not both.
	t.Run("one of the two fields recorded", func(t *testing.T) {
		out := runStatusOutput(t, func(st *state.State) {
			st.Network.Mode = "shared"
			st.Network.BridgeName = "kairoslab0"
		})
		if !strings.Contains(out, "bridge resources: bridge=kairoslab0 taps=none\n") {
			t.Errorf("status hides a bridge it recorded because the tap is missing:\n%s", out)
		}
	})
}

// The address row is printed in every mode, including the modes and the
// moments where there is no address: a missing row reads as a tool that
// forgot, and "none" is the answer that sends a user to the diagnostic the
// start printed.
func TestStatusAlwaysPrintsTheAddressRow(t *testing.T) {
	for _, mode := range []string{"shared", "bridged", "user", ""} {
		name := mode
		if name == "" {
			name = "nothing recorded"
		}
		t.Run(name, func(t *testing.T) {
			out := runStatusOutput(t, func(st *state.State) {
				st.Network.Mode = mode
				// A VM record with no address yet -- the row this test is
				// about is the per-VM one now (M5); with no VM at all there
				// is nothing to report an address for, which is a different
				// case than this test covers.
				withVMField(testVMName, func(v *state.VM) { v.NetworkMode = mode })(st)
			})
			if !strings.Contains(out, "  vm ip address: none\n") {
				t.Errorf("status does not report an unset address as none:\n%s", out)
			}
		})
	}
}

// The address row is the durable copy of what a start resolved, and a user
// reading it has less context than one who watched the start: the block that
// explained the address has scrolled away or was never theirs to see. So a
// self-assigned address is qualified in the row itself, and an address from
// a DHCP server is left exactly as it was.
func TestStatusQualifiesALinkLocalAddressRow(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want string
	}{
		{
			name: "a lease is printed as the bare address it is",
			ip:   "192.168.64.12",
			want: "vm ip address: 192.168.64.12\n",
		},
		{
			name: "a self-assigned address carries the reason it will not work",
			ip:   "169.254.11.9",
			want: "vm ip address: 169.254.11.9 (link-local - self-assigned because no DHCP server answered; not reachable)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runStatusOutput(t, func(st *state.State) {
				st.Network.Mode = "shared"
				withVMField(testVMName, func(v *state.VM) { v.IPAddress = tt.ip })(st)
			})
			// The trailing newline is the assertion for the first case: a
			// note appended to a routable address would not match.
			if !strings.Contains(out, tt.want) {
				t.Errorf("status prints no row %q:\n%s", tt.want, out)
			}
		})
	}
}

// --- status is a render boundary too ---------------------------------------

// The values `status` prints come from the same 0644 state.json the reset and
// cleanup plans read, and they used to be printed with a bare %s.
//
// The bridge interface is the field the reproduction used: "eth0", CSI 2K
// (erase the line), CSI 1G (back to column one), a newline and a row shaped
// like the one `status` prints. What a terminal does with that is print a
// forged "vm running: true" above the real "vm running: false" and erase the
// row the payload arrived on, so nothing is left to show where it came from.
const (
	injectedBridgeIface = "eth0\x1b[2K\x1b[1G\nvm running: true"
	injectedIPAddress   = "192.168.64.12\nvm running: true\x1b[2K\r"
	injectedISOSource   = "github\nvm running: true\x1b[2K\r"
	injectedISOLocal    = "/nope/kairos.iso\nvm running: true\x1b[2K\r"
	injectedLastError   = "exit status 1\nvm running: true\x1b[2K\r"
	injectedPackageMgr  = "apt\nvm running: true\x1b[2K\r"
	// The platform row is two stored fields joined with a slash, so both
	// halves are poisoned and what the row has to escape is the join.
	injectedPlatformOS   = "linux\nvm running: true\x1b[2K\r"
	injectedPlatformArch = "amd64\nvm running: true\x1b[2K\r"
)

// Every state-derived row of `status`, poisoned at once and in the same
// output: the previous fix at this boundary escaped two rows of one plan and
// the identical attack then walked through their siblings.
func TestStatusIsInertForEveryStoredValueItPrints(t *testing.T) {
	poisoned := map[string]string{
		"network.bridge_interface": injectedBridgeIface,
		"network.bridge_name":      injectedBridgeName,
		"network.tap_name":         injectedTapName,
		"vm.ip_address":            injectedIPAddress,
		"vm.iso_source":            injectedISOSource,
		"vm.iso_local_path":        injectedISOLocal,
		"vm.disk_path":             injectedDiskPath,
		"vm.last_error":            injectedLastError,
		"platform.package_manager": injectedPackageMgr,
		// One key for two fields, because runStatus prints them as one
		// value: platformLabel is os + "/" + arch, and it is that joined
		// string the row escapes or does not.
		"platform.os + platform.arch": injectedPlatformOS + "/" + injectedPlatformArch,
		"setup.pre_existing_deps":     injectedDepName,
		"managed_files":               injectedManagedFile,
		"managed_dirs":                injectedManagedDir,
	}
	out := runStatusOutput(t, func(st *state.State) {
		// bridged, so the two rows that are gated on a mode are printed too.
		st.Network.Mode = "bridged"
		st.Network.BridgeInterface = injectedBridgeIface
		st.Network.BridgeName = injectedBridgeName
		st.Network.TapName = injectedTapName
		withVMField(testVMName, func(v *state.VM) { v.IPAddress = injectedIPAddress })(st)
		withVMField(testVMName, func(v *state.VM) { v.ISOSource = injectedISOSource })(st)
		withVMField(testVMName, func(v *state.VM) { v.ISOLocal = injectedISOLocal })(st)
		withVMField(testVMName, func(v *state.VM) { v.DiskPath = injectedDiskPath })(st)
		withVMField(testVMName, func(v *state.VM) { v.LastError = injectedLastError })(st)
		withVMField(testVMName, func(v *state.VM) { v.TapName = injectedTapName })(st)
		withVMField(testVMName, func(v *state.VM) { v.NetworkMode = "bridged" })(st)
		st.Platform.OS = injectedPlatformOS
		st.Platform.Arch = injectedPlatformArch
		st.Platform.PackageManager = injectedPackageMgr
		st.Setup.PreExistingDeps = []string{injectedDepName}
		st.ManagedFiles = append(st.ManagedFiles, injectedManagedFile)
		st.ManagedDirs = append(st.ManagedDirs, injectedManagedDir)
	})

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("status output carries a raw escape byte, so a stored value reached the terminal:\n%q", out)
	}
	// The forged row, and the real one it was meant to replace. A raw newline
	// is what makes a forged row a row; after escaping the same text is one
	// quoted value on a single line.
	if strings.Contains(out, "\nvm running: true") {
		t.Errorf("status output carries a forged row:\n%q", out)
	}
	if !strings.Contains(out, "  vm running: false\n") {
		t.Errorf("status no longer reports whether the VM is running:\n%s", out)
	}
	// Counted as ROWS and not as occurrences: an escaped payload still
	// carries the words "vm running: true", harmlessly, in the middle of a
	// quoted value. What may never happen twice is a LINE that starts with
	// them (after its own indent), because that is the row a terminal would
	// show.
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "vm running:") {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("status printed %d vm-running rows, want exactly 1:\n%q", rows, out)
	}
	// Inert is half of it: the row still has to NAME what it is about, or a
	// status nobody can read has replaced one that lies.
	for field, value := range poisoned {
		if !strings.Contains(out, strconv.Quote(value)) {
			t.Errorf("the row carrying %s does not show the stored value in escaped form:\n%q", field, out)
		}
	}
}

// The same payload in the one field that decides which rows are printed at
// all. A mode no version of this CLI accepts prints no bridge rows, so what
// is asserted here is the row that is always printed.
// A VM's own network mode is validated at Load now (M1), which is a
// stronger defence than the escaping this test used to require: an invalid
// mode is quarantined before status has any row left to print it in, rather
// than reaching one that has to escape it.
func TestStatusIsInertForAStoredNetworkMode(t *testing.T) {
	out := runStatusOutput(t, func(st *state.State) {
		withVMField(testVMName, func(v *state.VM) { v.NetworkMode = injectedNetworkMode })(st)
	})
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("status output carries a raw escape byte:\n%q", out)
	}
	// A real newline followed by the forged row is what would make it read
	// as a row; the same text after an escaped "\\n" (inside the quoted
	// quarantine reason) is safe, and is what this now shows instead.
	if strings.Contains(out, "\n  - kairoslab9 (will be KEPT)") {
		t.Errorf("the poisoned network mode reached status output as a real forged row:\n%q", out)
	}
	if !strings.Contains(out, "quarantined vm record:") {
		t.Errorf("a VM with an invalid network mode should be quarantined and named as such, not silently dropped:\n%q", out)
	}
}

// An ordinary status is not quoted. Escaping everything would be a status
// nobody reads, which is the cost the plan rows were measured against.
func TestOrdinaryStatusRowsAreNotQuoted(t *testing.T) {
	out := runStatusOutput(t, func(st *state.State) {
		st.Network.Mode = "shared"
		st.Network.BridgeName = "kairoslab0"
		st.Network.TapName = "kairoslab-tap0"
		withVMField(testVMName, func(v *state.VM) { v.DiskPath = "/home/u/.cache/kairos-lab/vm/kairos-disk0.qcow2" })(st)
		withVMField(testVMName, func(v *state.VM) { v.IPAddress = "192.168.64.12" })(st)
		withVMField(testVMName, func(v *state.VM) { v.TapName = "kairoslab-tap0" })(st)
	})
	for _, want := range []string{
		"  disk path: /home/u/.cache/kairos-lab/vm/kairos-disk0.qcow2\n",
		"bridge resources: bridge=kairoslab0 taps=kairoslab-tap0\n",
		"  vm ip address: 192.168.64.12\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status row %q is not printed as plain text; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"kairoslab0"`) || strings.Contains(out, `"/home/u/`) {
		t.Errorf("an ordinary status came out quoted:\n%s", out)
	}
}

// fakeQEMUOnPath puts a qemu-system-* on PATH that prints a couple of lines
// the way a guest's serial console does and exits 0, and nothing else.
//
// It is what makes the whole of runStart reachable in a test: everything
// about the address lives after command.Start(), which PATH isolation alone
// never gets to. /bin/sh is named absolutely in the shebang, so the kernel
// finds the interpreter whatever PATH says, and the body is builtins only.
//
// The console lines matter as much as the exit code. -serial mon:stdio is on
// all four display branches in internal/vm, so a real guest's boot output
// arrives on the same stream the address block is printed to -- through
// os/exec's copier goroutine, which runs beside the poller. Writing nothing
// here would leave that pairing untested and the race detector with nothing
// to see.
//
// Both architectures' names are written because vm.BuildQEMUCommand picks by
// GOARCH, and a directory holding only these two is still the isolation the
// rest of this file relies on: no qemu-img, no ip, no brew.
func fakeQEMUOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor i in 1 2 3 4 5 6 7 8\ndo\n  echo \"[    0.00000$i] kairos boot console\"\ndone\nexit 0\n"
	for _, name := range []string{"qemu-system-x86_64", "qemu-system-aarch64"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
}

// The whole of part A through runStart: a VM is started, the address is
// looked up beside it, printed, and left in state.json for `status` to show.
//
// The last part is the one with a way of quietly failing. runStart blocks in
// command.Wait() holding the state it loaded before the VM started, and saves
// it again once the VM exits; the poller writes the address through a load
// and save of its own, so without the hand-back after the join that final
// save puts the field back to empty and `status` reports none for a VM whose
// address was printed on screen a moment earlier.
func TestStartResolvesTheAddressBesideTheVMAndLeavesItForStatus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test drives the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it launches anything, and this test's isolation from host binaries denies it one", runtime.GOOS)
	}
	const diskName = "kairos-disk0"
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	fakeQEMUOnPath(t)
	stubLinuxARM64Firmware(t)
	seedStartableState(t, diskName)
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t, "kairos-fake-uplink0")
	stubPrepareLinuxBridge(t, func(st *state.State, _ string, _ int, _ bool) error {
		// The fields vm.PrepareLinuxBridge writes on success. The tap name
		// is the one the QEMU command line needs to exist at all.
		st.Network.Mode = "bridged"
		st.Network.BridgeName = vm.DefaultBridgeName
		st.Network.TapName = vm.DefaultTapName
		st.Network.CleanupRequired = true
		st.Network.CreatedByKairosLab = true
		return nil
	})

	found := vm.IPResult{IP: "192.168.64.12", Source: vm.IPSourceARP}
	var asked vm.IPLookup
	var askedTimeout, askedInterval time.Duration
	stubIPPoll(t, func(_ context.Context, lookup vm.IPLookup, timeout, interval time.Duration) (vm.IPResult, bool) {
		// Read back in this test after Run returns, which is after runStart
		// has joined this goroutine.
		asked, askedTimeout, askedInterval = lookup, timeout, interval
		return found, true
	})

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"start", "-name", diskName, "-no-iso", "-network", "bridged", "-yes"},
		strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
		t.Fatalf("start returned %v; stdout:\n%s", err, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "vm exited") {
		t.Fatalf("the run did not reach the end of the VM's life, so nothing here was exercised:\n%s", out)
	}
	if !strings.Contains(out, vmUpBlock(found)) {
		t.Errorf("the address was never printed; got:\n%s", out)
	}
	// The guest's console shares this stream, written from os/exec's copier
	// goroutine while the poller prints into it. Both being here is what the
	// race detector needs to see to have an opinion about the lock they go
	// through.
	if !strings.Contains(out, "kairos boot console") {
		t.Errorf("the VM's console output never reached the stream the address is printed to:\n%s", out)
	}

	// The lookup is built out of what this run chose: the disk's own address,
	// the mode it settled on, the bridge the prepare reported and the
	// guest-agent socket QEMU was given.
	want := vm.IPLookup{
		MAC:           vm.MACForDisk(diskName),
		Mode:          "bridged",
		BridgeName:    vm.DefaultBridgeName,
		QGASocketPath: filepath.Join(runtimeDirForTest(t), "qemu.sock"),
	}
	if asked != want {
		t.Errorf("the poll was asked for %+v, want %+v", asked, want)
	}
	if askedTimeout != vm.DefaultIPPollTimeout || askedInterval != vm.DefaultIPPollInterval {
		t.Errorf("the poll ran for %v every %v, want the package defaults %v and %v",
			askedTimeout, askedInterval, vm.DefaultIPPollTimeout, vm.DefaultIPPollInterval)
	}

	st := loadStoredState(t)
	if soleVM(t, st).IPAddress != found.IP {
		t.Errorf("state records the address %q, want %q -- the save after the VM exited blanked what the poller wrote", soleVM(t, st).IPAddress, found.IP)
	}
	if soleVM(t, st).PID != 0 || soleVM(t, st).StoppedAt == "" {
		t.Errorf("the exit was not recorded: pid %d, stopped at %q", soleVM(t, st).PID, soleVM(t, st).StoppedAt)
	}

	// And the durable channel really shows it, which is the reason any of it
	// is persisted: the block above shares a terminal with the guest's boot
	// console and can scroll past unread.
	var statusOut bytes.Buffer
	if err := Run([]string{"status"}, strings.NewReader(""), &statusOut, &stderr, "test"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(statusOut.String(), "vm ip address: "+found.IP+"\n") {
		t.Errorf("status does not show the address the start resolved:\n%s", statusOut.String())
	}
}

// The warning a failed recording prints claims no more than BOTH of its
// failures keep.
//
// recordVMIP is a store.Load followed by a store.Save, and the two fail into
// different futures. A state file this process cannot PARSE is rebuilt by the
// save runStart makes when the VM exits -- that save writes the file from the
// state runStart is holding rather than reading the one the poller choked on
// -- so the address lands after all. A state file this process cannot WRITE
// fails at the same store.Save the exit will run, on the same store, so the
// exit fails identically and nothing is ever recorded. The first line of the
// warning holds either way: the window the poll exists to serve is the one
// that loses the address. The second line is why this test has two halves --
// as a promise it was false of the write half, so it says the run tries.
//
// Both halves produce their failure rather than simulating it, one layer
// apart: the first corrupts the state file, the second takes the write
// permission off the directory store.Save creates its temporary file in.
// That second one stands in for any filesystem that will not take the write
// -- a full disk, a quota, a read-only remount -- which fail at their own
// syscall inside store.Save and leave the caller where this one does.
func TestAFailedRecordingClaimsNoMoreThanBothItsFailuresKeep(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test drives the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it launches anything, and the PATH this test sets holds nothing but a fake qemu", runtime.GOOS)
	}
	const diskName = "kairos-disk0"
	found := vm.IPResult{IP: "192.168.64.12", Source: vm.IPSourceARP}

	t.Run("the state file cannot be parsed", func(t *testing.T) {
		store := startableBridgedRun(t, diskName)
		// Written by the poll goroutine and read after Run returns, which is
		// after runStart has joined that goroutine.
		var statusAtWarningTime string
		var statusErrAtWarningTime error
		stubIPPoll(t, func(context.Context, vm.IPLookup, time.Duration, time.Duration) (vm.IPResult, bool) {
			// recordVMIP loads the state file itself, and this is a file it
			// cannot parse.
			if err := os.WriteFile(store.StatePath, []byte("not json\n"), 0o644); err != nil {
				t.Errorf("corrupt the state file: %v", err)
				return vm.IPResult{}, false
			}
			// What a `status` in another terminal met at the moment the
			// warning was printed. It runs from this goroutine, before the
			// answer below has reached runStart and before anything else
			// saves.
			var out bytes.Buffer
			statusErrAtWarningTime = Run([]string{"status"}, strings.NewReader(""), &out, io.Discard, "test")
			statusAtWarningTime = out.String()
			return found, true
		})

		var stdout, stderr bytes.Buffer
		if err := Run([]string{"start", "-name", diskName, "-no-iso", "-network", "bridged", "-yes"},
			strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
			t.Fatalf("start returned %v; stdout:\n%s", err, stdout.String())
		}
		warning := stderr.String()
		assertRecordingWarningClaimsAnAttempt(t, warning)
		if !strings.Contains(warning, "parse state file") {
			t.Fatalf("the warning does not name the load as what failed, so this half is not the one it is written for:\n%s", warning)
		}

		// What the user met at that moment is the corruption this test
		// introduced: `status` loads the same file and fails on it. The
		// assertion is on the error and not on the empty output it left
		// behind, because "the output does not contain the address" is true
		// of an empty string no production change could fill.
		if statusErrAtWarningTime == nil {
			t.Errorf("`status` succeeded on a state file it cannot parse, printing:\n%s", statusAtWarningTime)
		} else if !strings.Contains(statusErrAtWarningTime.Error(), "parse state file") {
			t.Errorf("`status` failed with %v, want the parse failure the corrupt file causes", statusErrAtWarningTime)
		}

		// And the half the second line is about: the exit save rebuilt the
		// file it could not read, so the address is on record afterwards.
		if got := soleVM(t, loadStoredState(t)).IPAddress; got != found.IP {
			t.Fatalf("state records the address %q, want %q -- the exit save did not put back what the poller could not write", got, found.IP)
		}
		var statusOut bytes.Buffer
		if err := Run([]string{"status"}, strings.NewReader(""), &statusOut, io.Discard, "test"); err != nil {
			t.Fatalf("status: %v", err)
		}
		if !strings.Contains(statusOut.String(), "vm ip address: "+found.IP+"\n") {
			t.Errorf("`status` after the VM exited does not show the address the run went on to write:\n%s", statusOut.String())
		}
	})

	t.Run("the state file cannot be written", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("this half needs a directory the process may not create a file in, and root may create one in any directory whatever its mode says")
		}
		store := startableBridgedRun(t, diskName)
		// The mode is put back after the start below, before anything reads
		// the state file back. This cleanup is the belt for the t.Fatal
		// paths: the config dir lives inside a t.TempDir(), and that
		// directory's own cleanup cannot unlink through mode 0500 either.
		t.Cleanup(func() { _ = os.Chmod(store.ConfigDir, 0o755) })
		var statusAtWarningTime string
		var statusErrAtWarningTime error
		stubIPPoll(t, func(context.Context, vm.IPLookup, time.Duration, time.Duration) (vm.IPResult, bool) {
			// The file still parses; it is store.Save that cannot create its
			// temporary file beside it.
			if err := os.Chmod(store.ConfigDir, 0o500); err != nil {
				t.Errorf("take the write permission off the config dir: %v", err)
				return vm.IPResult{}, false
			}
			var out bytes.Buffer
			statusErrAtWarningTime = Run([]string{"status"}, strings.NewReader(""), &out, io.Discard, "test")
			statusAtWarningTime = out.String()
			return found, true
		})

		var stdout, stderr bytes.Buffer
		startErr := Run([]string{"start", "-name", diskName, "-no-iso", "-network", "bridged", "-yes"},
			strings.NewReader(""), &stdout, &stderr, "test")
		if err := os.Chmod(store.ConfigDir, 0o755); err != nil {
			t.Fatalf("restore the config dir mode: %v", err)
		}
		warning := stderr.String()
		assertRecordingWarningClaimsAnAttempt(t, warning)
		if !strings.Contains(warning, "create temporary state file") {
			t.Fatalf("the warning does not name the save as what failed, so this half is not exercising the save:\n%s", warning)
		}

		// A `status` at that moment reads a file that is still whole, so it
		// succeeds -- and shows no address, which is the listing the first
		// line of the warning describes.
		if statusErrAtWarningTime != nil {
			t.Errorf("`status` failed with %v, and this half leaves the state file readable", statusErrAtWarningTime)
		} else if !strings.Contains(statusAtWarningTime, "vm ip address: none\n") {
			t.Errorf("`status` at the moment of the warning does not report an address of none:\n%s", statusAtWarningTime)
		}

		// The exit save is the same store.Save against the same unwritable
		// directory, so it fails too: the address is never recorded, and the
		// start says so rather than finishing quietly. This is the
		// measurement the second line of the warning is worded for -- a
		// promise to write the address later would be false here.
		if startErr == nil {
			t.Fatalf("the start succeeded although no save could be written; stdout:\n%s", stdout.String())
		}
		if !strings.Contains(startErr.Error(), "create temporary state file") {
			t.Errorf("the start failed with %v, want the save failure the unwritable config dir causes", startErr)
		}
		if got := soleVM(t, loadStoredState(t)).IPAddress; got != "" {
			t.Errorf("state records the address %q after a run that could write nothing", got)
		}
		var statusOut bytes.Buffer
		if err := Run([]string{"status"}, strings.NewReader(""), &statusOut, io.Discard, "test"); err != nil {
			t.Fatalf("status: %v", err)
		}
		if !strings.Contains(statusOut.String(), "vm ip address: none\n") {
			t.Errorf("`status` after the VM exited shows an address the run never managed to write:\n%s", statusOut.String())
		}
	})
}

// assertRecordingWarningClaimsAnAttempt checks the text both halves above
// share: the failure is announced, the loss is scoped to the window the VM is
// running in, and the later write is offered as an attempt.
//
// The last two checks are a pair. The unscoped "will not show it: " was
// contradicted by the same run moments later on the parse half; "this run
// writes the address ... again" was contradicted on the write half, where the
// exit save fails exactly as the poller's did. Both are rejected by name, so
// re-strengthening either sentence reddens both halves.
func assertRecordingWarningClaimsAnAttempt(t *testing.T, warning string) {
	t.Helper()
	if !strings.Contains(warning, "warning: the VM address was not recorded") {
		t.Fatalf("the failed recording said nothing:\n%s", warning)
	}
	if !strings.Contains(warning, "will not show it while this VM is running") {
		t.Errorf("the warning does not scope its claim to the window it holds in:\n%s", warning)
	}
	if strings.Contains(warning, "will not show it: ") {
		t.Errorf("the warning claims `status` will not show the address at all, which a run whose exit save succeeds goes on to contradict:\n%s", warning)
	}
	if !strings.Contains(warning, "tries to write the address to the state file again when the VM exits") {
		t.Errorf("the warning does not say what the run does next:\n%s", warning)
	}
	if strings.Contains(warning, "run writes the address to the state file again") {
		t.Errorf("the warning promises the later write, which a run whose config dir cannot be written does not keep:\n%s", warning)
	}
}

// startableBridgedRun is the setup the halves above share: a Linux bridged
// start that reaches command.Wait() with a fake QEMU on PATH and the host
// bridge preparation stubbed out, and the store whose state file the run
// writes.
func startableBridgedRun(t *testing.T, diskName string) *state.Store {
	t.Helper()
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	fakeQEMUOnPath(t)
	stubLinuxARM64Firmware(t)
	seedStartableState(t, diskName)
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t, "kairos-fake-uplink0")
	stubPrepareLinuxBridge(t, func(st *state.State, _ string, _ int, _ bool) error {
		st.Network.Mode = "bridged"
		st.Network.BridgeName = vm.DefaultBridgeName
		st.Network.TapName = vm.DefaultTapName
		st.Network.CleanupRequired = true
		st.Network.CreatedByKairosLab = true
		return nil
	})
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	return store
}

// What the child process writes through, per stream, and why the sudo branch
// is a question about the object and not about the mode.
//
// os/exec starts no copier goroutine for an *os.File -- the descriptor is
// handed to the child -- so the address poller writing through the wrapper is
// then the only Go-level writer of that stream. For any other io.Writer it
// starts one, and that goroutine writes the unwrapped object while the poller
// writes the wrapper: two writers, no shared lock, which is a data race on
// the writer's own state -- the race detector reports it as one, on the
// mutant with the type test taken out.
//
// The branch is latent rather than live: it needs darwin and a vmnet mode,
// and main passes os.Stdout. This test is what pins it before the next darwin
// test that gets past command.Start() with a buffer inherits the race.
func TestTheChildKeepsTheRawStreamOnlyWhenItIsAFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	var buf bytes.Buffer
	wrapped := &syncWriter{w: &buf}

	if got := childStdio(true, f, wrapped); got != io.Writer(f) {
		t.Errorf("childStdio(sudo, *os.File) = %T, want the file itself: proxying it through a pipe is what sudo on macOS fails to allocate a pty for", got)
	}
	if got := childStdio(true, &buf, wrapped); got != io.Writer(wrapped) {
		t.Errorf("childStdio(sudo, *bytes.Buffer) = %T, want the wrapped writer: os/exec copies into anything that is not an *os.File from a goroutine of its own, which shares no lock with the poller", got)
	}
	if got := childStdio(false, f, wrapped); got != io.Writer(wrapped) {
		t.Errorf("childStdio(not sudo, *os.File) = %T, want the wrapped writer -- the log file hangs off it", got)
	}

	// The property those choices exist for, exercised the way the race
	// detector can see it: the writer the child would use and the writer the
	// poller uses, written at the same time. With the type test in place both
	// are the one syncWriter; without it the first is this buffer, raw.
	child := childStdio(true, &buf, wrapped)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = child.Write([]byte("[    0.000001] kairos boot console\n"))
		}()
		go func() {
			defer wg.Done()
			_, _ = wrapped.Write([]byte("VM is up.\n"))
		}()
	}
	wg.Wait()
}

// runtimeDirForTest is the runtime directory a start uses under the cache
// directory the test set, which is where the guest-agent socket lives.
func runtimeDirForTest(t *testing.T) string {
	t.Helper()
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	return filepath.Join(store.CacheDir, "runtime")
}

// --- multi-VM (kairos-lab#6) -------------------------------------------

// TestStartAllocatesTheNextFreeIndexForASecondLiveVM is an end-to-end proof
// of the feature this issue is about: two differently-named VMs in one
// config dir get different indices, runtime paths and user-mode ports, and
// starting the second is not refused over the first. -network user is what
// lets this run on any host without NetworkManager or a real bridge: it
// takes neither of the two Linux-only prepare branches this package seams,
// so nothing here is faked, and PATH isolation makes both starts fail at
// the QEMU launch itself -- after "[2/3] Recording VM state" has already
// run, which is the state this test reads.
func TestStartAllocatesTheNextFreeIndexForASecondLiveVM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("this test drives the start path as Linux takes it, and %s is not Linux: on darwin -- the only other platform kairos-lab is built for -- the run needs a firmware path from `brew --prefix qemu` before it records anything, and this test's isolation from host binaries denies it one", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableStateWithDisks(t, "vm-a", "vm-b")

	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}

	var stdoutA, stderrA bytes.Buffer
	errA := Run([]string{"start", "-name", "vm-a", "-no-iso", "-network", "user", "-yes"},
		strings.NewReader(""), &stdoutA, &stderrA, "test")
	if errA == nil || !strings.Contains(errA.Error(), "start qemu") {
		t.Fatalf("starting vm-a returned %v, want it to have recorded state and then failed to launch; stdout:\n%s", errA, stdoutA.String())
	}

	// vm-a's own record carries no PID (the launch never got that far), so
	// VMIsLive's reservation half needs its StarterPID to still be a running
	// process for vm-b's start to see it as a live sibling and allocate
	// index 1 rather than reusing index 0. This process's own PID is
	// guaranteed to still be running.
	if err := store.Update(func(st *state.State) error {
		v := state.FindVM(st, "vm-a")
		if v == nil {
			t.Fatal("vm-a has no state record after its start")
		}
		v.StarterPID = os.Getpid()
		return nil
	}); err != nil {
		t.Fatalf("mark vm-a live: %v", err)
	}

	var stdoutB, stderrB bytes.Buffer
	errB := Run([]string{"start", "-name", "vm-b", "-no-iso", "-network", "user", "-yes"},
		strings.NewReader(""), &stdoutB, &stderrB, "test")
	if errB == nil || !strings.Contains(errB.Error(), "start qemu") {
		t.Fatalf("starting vm-b with vm-a live returned %v, want the same recorded-then-failed-launch outcome, not a refusal over vm-a; stdout:\n%s", errB, stdoutB.String())
	}

	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := state.FindVM(st, "vm-a")
	b := state.FindVM(st, "vm-b")
	if a == nil || b == nil {
		t.Fatalf("both VMs should have a state record: vm-a=%v vm-b=%v", a, b)
	}
	if a.Index != 0 {
		t.Errorf("vm-a index = %d, want 0 (the first VM started, with a live sibling check that had nothing else to find)", a.Index)
	}
	if b.Index != 1 {
		t.Errorf("vm-b index = %d, want 1 (vm-a is live at index 0, so the lowest free index is 1)", b.Index)
	}
	if a.QGASockPath == b.QGASockPath {
		t.Errorf("both VMs share a QGA socket path %q, so a second one would collide with the first", a.QGASockPath)
	}
	if a.LogPath == b.LogPath {
		t.Errorf("both VMs share a log path %q", a.LogPath)
	}
	if !strings.HasSuffix(a.QGASockPath, "qemu.sock") {
		t.Errorf("vm-a (index 0) QGA socket = %q, want it to end in the byte-identical index-0 name qemu.sock", a.QGASockPath)
	}
	if !strings.HasSuffix(b.QGASockPath, "qemu-1.sock") {
		t.Errorf("vm-b (index 1) QGA socket = %q, want it to end in qemu-1.sock", b.QGASockPath)
	}
	// User-mode ports: index 0 keeps the fixed 2222/8080, index 1 gets
	// 2223/8081. The recorded QEMU command line is where this is externally
	// observable.
	if !slices.Contains(a.QemuArgs, "user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080") {
		t.Errorf("vm-a's user-mode netdev is not the unchanged index-0 default:\n%q", a.QemuArgs)
	}
	if !slices.Contains(b.QemuArgs, "user,id=net0,hostfwd=tcp:127.0.0.1:2223-:22,hostfwd=tcp:127.0.0.1:8081-:8080") {
		t.Errorf("vm-b's user-mode netdev is not 2223/8081 bound to 127.0.0.1:\n%q", b.QemuArgs)
	}
}

// TestStatusShowsTwoVMsAndTwoAddresses is AC 3 (mutual reachability is a
// host-level claim this suite cannot make; that two VMs show up as two
// distinct, independently addressed entries in status is the part of it
// this package owns).
func TestStatusShowsTwoVMsAndTwoAddresses(t *testing.T) {
	out := runStatusOutput(t, func(st *state.State) {
		state.UpsertVM(st, state.VM{Name: "vm-a", Index: 0, IPAddress: "192.168.64.10", NetworkMode: "shared"})
		state.UpsertVM(st, state.VM{Name: "vm-b", Index: 1, IPAddress: "192.168.64.11", NetworkMode: "shared"})
	})
	for _, want := range []string{"vm: vm-a", "vm: vm-b", "  vm ip address: 192.168.64.10\n", "  vm ip address: 192.168.64.11\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not print %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "vm ip address:"); got != 2 {
		t.Errorf("status prints %d address rows, want exactly 2 -- one per VM:\n%s", got, out)
	}
}

// TestResetOneVMLeavesASiblingsRecordAlone is the state-layer half of "reset
// -disk a with b live never disturbs b": internal/vm's own fakeHost tests
// (network_multivm_linux_test.go) cover the network side -- that
// CleanupLinuxBridge with siblingLive=true never touches the bridge or a
// sibling's tap. This is the app-layer half, with no real networking
// involved: b's own VM record must survive a `reset -disk a` untouched.
func TestResetOneVMLeavesASiblingsRecordAlone(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatalf("DefaultStore: %v", err)
	}
	st := state.NewState(store)
	st.Setup.CompletedAt = state.NowRFC3339()
	st.Setup.DependencyCheckPassed = true
	state.AddDisk(st, state.Disk{Name: "vm-a", Path: "/nope/vm-a.qcow2", CreatedAt: state.NowRFC3339(), Size: "60G"})
	state.AddDisk(st, state.Disk{Name: "vm-b", Path: "/nope/vm-b.qcow2", CreatedAt: state.NowRFC3339(), Size: "60G"})
	state.UpsertVM(st, state.VM{Name: "vm-a", Index: 0})
	// vm-b is live: a real, running PID, so reset must refuse to touch it
	// and must not report it as part of a completed reset either.
	state.UpsertVM(st, state.VM{Name: "vm-b", Index: 1, PID: os.Getpid()})
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"reset", "-disk", "vm-a", "-yes"}, strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
		t.Fatalf("reset -disk vm-a: %v; stdout:\n%s", err, stdout.String())
	}

	got := loadStoredState(t)
	if state.FindVM(got, "vm-a") != nil {
		t.Error("vm-a's own record should have been removed by its own reset")
	}
	b := state.FindVM(got, "vm-b")
	if b == nil {
		t.Fatal("vm-b's record was removed by a reset that named only vm-a")
	}
	if b.PID != os.Getpid() {
		t.Errorf("vm-b's record was modified by a reset that named only vm-a: PID = %d, want %d", b.PID, os.Getpid())
	}
	if state.FindDiskByName(got, "vm-b") == nil {
		t.Error("vm-b's disk was removed by a reset that named only vm-a")
	}
}

// stubIsWiFiIface answers the Wi-Fi question with a fixed verdict for one
// test. The real detector stats /sys/class/net on Linux and shells out to
// networksetup on macOS, so it answers for whatever the suite host is plugged
// into -- a CI leg on Ethernet and a laptop on Wi-Fi would disagree about
// whether the caveat is printed, which is the one thing these two tests pin.
func stubIsWiFiIface(t *testing.T, wifi bool) {
	t.Helper()
	saved := isWiFiIface
	t.Cleanup(func() { isWiFiIface = saved })
	isWiFiIface = func(string) bool { return wifi }
}

// stubValidateBridgeIface answers the host link check with a fixed verdict.
//
// Every bridged `start` test below needs this, and on darwin it needs it to
// get anywhere at all: vm.ValidateBridgeIface is a no-op off darwin, but the
// darwin one reads `ifconfig -l` and rejects any name that is not on the
// host, so the run returns at "[1/3] Preparing networking" and prints
// nothing further. That is why these tests were green on the ubuntu leg and
// red on the macOS one.
func stubValidateBridgeIface(t *testing.T, err error) {
	t.Helper()
	saved := validateBridgeIface
	t.Cleanup(func() { validateBridgeIface = saved })
	validateBridgeIface = func(string) error { return err }
}

// A bridged run onto a Wi-Fi radio says so before it starts, on Linux as on
// macOS (kairos-io/kairos#5021).
//
// The warning used to sit inside a `runtime.GOOS == "darwin"` gate, next to
// the vmnet link check, and vm.IsWiFiIface was hardwired to false off darwin
// as well -- so both halves had to move for this to print, and restoring
// either one alone turns this test red.
//
// Linux is not the lesser case here, it is the worse one. On macOS the user
// has usually named the interface; on Linux resolveBridgeUplink takes the
// first of vm.DetectUplinkCandidates, which reads the default route, so a
// laptop on Wi-Fi gets the radio enslaved to a bridge without ever having
// chosen it. The run is allowed either way -- bridging over Wi-Fi works often
// enough to be worth allowing -- which is why this asserts a line of output
// and not an error.
func TestStartWarnsWhenTheBridgedUplinkIsWiFi(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("bridged networking has no host side on %s", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubFirmwareLookupAway(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t, "kairos-fake-wlan0")
	stubValidateBridgeIface(t, nil)
	stubIsWiFiIface(t, true)

	// Enter at the review, Enter to start, then refuse the privilege prompt,
	// so the run stops before anything touches the host.
	var stdout, stderr bytes.Buffer
	_ = Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "bridged"},
		scriptedInput("\n\nn\n"), &stdout, &stderr, "test")

	out := stdout.String()
	if !strings.Contains(out, "kairos-fake-wlan0 is Wi-Fi") {
		t.Errorf("a bridged start onto a Wi-Fi interface printed no caveat naming it; stdout:\n%s", out)
	}
	if !strings.Contains(out, "may never get a lease") {
		t.Errorf("the caveat does not say what goes wrong, which is the whole of its value; stdout:\n%s", out)
	}
}

// The negative control, and it is not a formality: the cheapest way to make
// the test above pass is to print the caveat for every bridged run, which
// would put a paragraph about access points in front of every user on
// Ethernet. A warning that is always printed is a warning nobody reads.
func TestStartDoesNotWarnWhenTheBridgedUplinkIsNotWiFi(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("bridged networking has no host side on %s", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubFirmwareLookupAway(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t, "kairos-fake-uplink0")
	stubValidateBridgeIface(t, nil)
	stubIsWiFiIface(t, false)

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "bridged"},
		scriptedInput("\n\nn\n"), &stdout, &stderr, "test")

	// An absence assertion is only worth as much as the proof that the run
	// reached the place the thing would have been printed, and this one very
	// nearly was not: on darwin the real vm.ValidateBridgeIface above returns
	// "no such interface kairos-fake-uplink0 on this host", runStart returns
	// it, and nothing after "[1/3] Preparing networking" is ever written. The
	// caveat's absence then says nothing at all.
	//
	// Assert on the step banner rather than on the shape of the error, which
	// is what the first version of this test did -- it matched the interface
	// name, on the reasoning that every message the validator can produce
	// names it and nothing raised later does. That was true and still let the
	// test pass for the wrong reason on macos-latest, where the run died at
	// "discover qemu brew prefix" two steps earlier and named no interface at
	// all (see stubFirmwareLookupAway above).
	//
	// The banner is printed by runStart immediately before the bridged block
	// that holds the caveat, so it cannot be reached by a run that stopped
	// short of it, whatever the run stopped of. An absence assertion is worth
	// exactly as much as the proof that the run got to where the thing would
	// have been printed, and this is that proof on every host.
	out := stdout.String()
	if !strings.Contains(out, "[1/3] Preparing networking") {
		t.Fatalf("the run never reached the networking step, so the caveat's absence proves nothing (err: %v); stdout:\n%s", err, out)
	}
	if strings.Contains(out, "is Wi-Fi") {
		t.Errorf("a bridged start onto an Ethernet interface printed the Wi-Fi caveat; stdout:\n%s", out)
	}
}

// The other half of the seam: a bridge interface the host cannot carry is a
// refusal, and it comes first, so a run that is about to be rejected does not
// also lecture the user about access points. The caveat and the refusal live
// one after the other in the same block and this is what pins their order.
//
// It is darwin-only because the carrier check is: vm.ValidateBridgeIface is
// `return nil` off darwin, and kairos-io/kairos#5021 deliberately left the
// Linux half of kairos-io/kairos#4431 unbuilt rather than guess at whether
// NetworkManager will activate a carrier-less ethernet slave.
func TestStartRefusesADeadBridgeIfaceBeforeWarningAboutWiFi(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("vm.ValidateBridgeIface is a no-op on %s, so there is no refusal to order the caveat against", runtime.GOOS)
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	stubFirmwareLookupAway(t)
	seedStartableState(t, "kairos-disk0")
	stubNetworkPrivilege(t, func(string) error { return nil })
	stubBridgeIfaceCandidates(t, "kairos-fake-wlan0")
	stubValidateBridgeIface(t, errors.New("bridge interface kairos-fake-wlan0 is inactive"))
	// True, so that a caveat printed here could only be one printed ahead of
	// the refusal rather than one the detector declined to raise.
	stubIsWiFiIface(t, true)

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "bridged"},
		scriptedInput("\n\nn\n"), &stdout, &stderr, "test")

	if err == nil || !strings.Contains(err.Error(), "is inactive") {
		t.Fatalf("start returned %v, want the bridge interface refusal -- the seam is not wired into runStart; stdout:\n%s", err, stdout.String())
	}
	if out := stdout.String(); strings.Contains(out, "is Wi-Fi") {
		t.Errorf("the Wi-Fi caveat was printed for a run that was then refused anyway; stdout:\n%s", out)
	}
}

// shared and user attach to no physical interface, so neither can be bridged
// onto a radio and neither may carry the caveat. shared is the one that
// matters: it is the DEFAULT mode, it is the mode that works over Wi-Fi
// precisely because no guest frame leaves the host with a MAC the access
// point did not see associate, and a caveat there would tell the user the
// opposite of the truth.
func TestStartDoesNotWarnAboutWiFiOutsideBridgedMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("shared networking is prepared by NetworkManager, which is Linux-only; on %s it is vmnet-shared", runtime.GOOS)
	}
	for _, mode := range []string{"shared", "user"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			isolateFromHostBinaries(t)
			seedStartableState(t, "kairos-disk0")
			stubNetworkPrivilege(t, func(string) error { return nil })
			stubBridgeIfaceCandidates(t, "kairos-fake-wlan0")
			stubIsWiFiIface(t, true)

			var stdout, stderr bytes.Buffer
			_ = Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", mode, "-bridge-if", "kairos-fake-wlan0"},
				scriptedInput("\n\nn\n"), &stdout, &stderr, "test")

			if out := stdout.String(); strings.Contains(out, "is Wi-Fi") {
				t.Errorf("a %s start printed the Wi-Fi caveat, which describes a bridge it never builds; stdout:\n%s", mode, out)
			}
		})
	}
}

// The seam has to BE vm.IsWiFiIface, because every test above replaces it.
// A default that had drifted to a local `return false` -- which is exactly
// what vm.IsWiFiIface itself was off darwin before kairos-io/kairos#5021 --
// would leave all three of them green while no real run ever warned.
//
// Comparing the function values is what makes that assertable, and it is why
// the seam is a direct assignment rather than a closure: a closure has its
// own code pointer, so this comparison could not tell one wrapping
// vm.IsWiFiIface from one wrapping a constant.
func TestWiFiSeamIsTheRealDetector(t *testing.T) {
	if got, want := reflect.ValueOf(isWiFiIface).Pointer(), reflect.ValueOf(vm.IsWiFiIface).Pointer(); got != want {
		t.Error("the app layer's Wi-Fi seam is not vm.IsWiFiIface, so what a real run asks is not what the tests pin")
	}
}

// And the same for the validator beside it, which every bridged test above
// now replaces. A default that had drifted to `func(string) error { return
// nil }` would leave them all green while a real macOS run bridged onto a
// dead port, which is the failure kairos-io/kairos#4431 was filed for.
func TestBridgeIfaceSeamIsTheRealValidator(t *testing.T) {
	if got, want := reflect.ValueOf(validateBridgeIface).Pointer(), reflect.ValueOf(vm.ValidateBridgeIface).Pointer(); got != want {
		t.Error("the app layer's bridge interface seam is not vm.ValidateBridgeIface, so what a real run checks is not what the tests pin")
	}
}

// stubStaleNetwork makes the "stale network resources" branch of the reset
// and cleanup plans the one that runs. See the comment on
// hasStaleNetworkResources for why a test cannot reach it otherwise.
func stubStaleNetwork(t *testing.T, stale bool) {
	t.Helper()
	saved := hasStaleNetworkResources
	t.Cleanup(func() { hasStaleNetworkResources = saved })
	hasStaleNetworkResources = func(_ *state.State) bool { return stale }
}

// The stale branch resolved nothing. It printed kairoslab0 four times and
// named no tap at all, while vm.CleanupStaleNetworkResources -- the call the
// user is consenting to -- reads st.Network.BridgeName and falls back to the
// default only when it is empty, then deletes the bridge link, the tap link
// and three connections built from it. A stored bridge therefore had the plan
// naming interfaces the teardown would not touch, and the teardown deleting
// interfaces, connections and links the plan never named. Correcting a stored
// bridge name is not a hypothetical, either: it is what reset's own "reset
// incomplete" error tells the user to do.
//
// The sibling branch a few lines up, the CreatedByKairosLab one, resolves its
// names and has done since it was written. This asserts the stale branch says
// what its own teardown will do.
func TestStalePlanNamesTheResourcesItsCleanupWouldTouch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the network plan rows are only printed on linux")
	}
	const storedBridge = "mylab0"
	for _, verb := range []string{"reset", "cleanup"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			seedInjectedState(t, func(st *state.State) {
				st.Network.BridgeName = storedBridge
				// A tap name a removed mode left in the file. The teardown
				// derives its tap from the VM index and ignores this, so the
				// plan must ignore it too: naming it would be the same bug
				// in the other direction.
				st.Network.TapName = "mylab-vtap9"
				// What puts the plan on the stale branch rather than the
				// CreatedByKairosLab one.
				st.Network.CreatedByKairosLab = false
			})
			stubStaleNetwork(t, true)

			var stdout, stderr bytes.Buffer
			if err := Run([]string{verb, "-dry-run"}, strings.NewReader(""), &stdout, &stderr, "test"); err != nil {
				t.Fatalf("%s -dry-run: %v", verb, err)
			}
			out := stdout.String()

			// Every name cleanupNMConnections builds for index 0 out of the
			// stored bridge: the bridge link, the tap link, and the three
			// connections. Every row is matched whole, down to its newline:
			// a row naming "mylab0-tapX" contains "mylab0-tap", so a
			// substring check would pass for a name the teardown never
			// touches.
			for _, want := range []string{
				"bridge: " + storedBridge + "\n",
				"tap: " + vm.TapNameForIndex(0) + "\n",
				"connections: " + storedBridge + ", " + storedBridge + "-uplink, " + vm.TapConnNameForIndex(storedBridge, 0) + "\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("plan does not name %q, which the cleanup would act on; got:\n%s", want, out)
				}
			}
			// And it must not name the default bridge, which this teardown
			// will leave alone, nor the stored tap, which it never reads.
			for _, unwanted := range []string{vm.DefaultBridgeName, "mylab-vtap9"} {
				if strings.Contains(out, unwanted) {
					t.Errorf("plan names %q, which the cleanup would not touch; got:\n%s", unwanted, out)
				}
			}
		})
	}
}

// The names the plan prints and the names the cleanup acts on are now one
// resolution, vm.StaleNetworkResourceNames, rather than two copies of the
// same defaulting rule. This pins the rule itself, including the half the
// stale plan used to get wrong: a stored bridge wins only when the recorded
// mode is one that builds a bridge of its own, anything else falls back to
// the default, and both tap names follow from whichever bridge won.
func TestStaleNetworkResourceNamesResolvesStoredNamesThenDefaults(t *testing.T) {
	for _, tc := range []struct {
		name              string
		mode, bridge, tap string
		wantBridge        string
		wantTapConn       string
	}{
		{"nothing stored", "shared", "", "", vm.DefaultBridgeName, vm.DefaultBridgeName + "-tap"},
		{"a bridge stored", "shared", "mylab0", "", "mylab0", "mylab0-tap"},
		{"a bridge stored by the bridged mode", "bridged", "mylab0", "", "mylab0", "mylab0-tap"},
		// A tap left by a mode that recorded one. The teardown derives the
		// tap from the VM index, so a stored tap changes neither name.
		{"a tap stored too", "shared", "mylab0", "mylab-vtap9", "mylab0", "mylab0-tap"},
		{"only a tap stored", "shared", "", "mylab-vtap9", vm.DefaultBridgeName, vm.DefaultBridgeName + "-tap"},
		// The reason the rule asks about the mode at all. `--network virbr`
		// is gone, but the state.json it wrote records libvirt's own bridge
		// in the same field the surviving modes read back and then delete.
		// Resolving it here would have this cleanup take libvirt's default
		// network down for every VM on the host.
		{"a virbr bridge is not ours to name", "virbr", "virbr0", "vnet3", vm.DefaultBridgeName, vm.DefaultBridgeName + "-tap"},
		// And an unset mode is not a claim of ownership either: it is what
		// a state file written before the field existed looks like.
		{"no mode recorded", "", "virbr0", "", vm.DefaultBridgeName, vm.DefaultBridgeName + "-tap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{}
			st.Network.Mode = tc.mode
			st.Network.BridgeName = tc.bridge
			st.Network.TapName = tc.tap
			bridge, tap, tapConn := vm.StaleNetworkResourceNames(st)
			if bridge != tc.wantBridge || tapConn != tc.wantTapConn {
				t.Errorf("got bridge=%q tapConn=%q, want bridge=%q tapConn=%q",
					bridge, tapConn, tc.wantBridge, tc.wantTapConn)
			}
			// The tap DEVICE is index 0's whatever state says, which is what
			// makes a stored tap unreachable from this path.
			if tap != vm.TapNameForIndex(0) {
				t.Errorf("got tap=%q, want index 0's %q", tap, vm.TapNameForIndex(0))
			}
		})
	}
}

// A pipe, a heredoc and a redirected file all hand back as much as is ready
// rather than one line per Read, and that is what broke kairos-io/kairos#5057:
// the review menu buffered the whole script, used line one, and every prompt
// after it read EOF. strings.Reader has exactly that behaviour, so these
// specs fail against a reviewer that buffers more than once.
func TestReviewVMConfigTakesEveryAnswerFromPipedStdin(t *testing.T) {
	cfg := reviewableConfig(t)
	var stdout bytes.Buffer

	got, err := reviewVMConfig(cfg, strings.NewReader("5\n8\n\n"), &stdout)
	if err != nil {
		t.Fatalf("reviewVMConfig on piped stdin: %v", err)
	}
	if got.MemoryGB != 8 {
		t.Fatalf("memory: got %d GB, want 8 GB", got.MemoryGB)
	}
}

// Several edits in one script: the failure only showed from the second prompt
// onwards, so one edit is not enough to pin it.
func TestReviewVMConfigTakesSeveralEditsFromPipedStdin(t *testing.T) {
	cfg := reviewableConfig(t)
	var stdout bytes.Buffer

	got, err := reviewVMConfig(cfg, strings.NewReader("5\n8\n6\n4\n7\nshared\n\n"), &stdout)
	if err != nil {
		t.Fatalf("reviewVMConfig on piped stdin: %v", err)
	}
	if got.MemoryGB != 8 {
		t.Fatalf("memory: got %d GB, want 8 GB", got.MemoryGB)
	}
	if got.CPUs != 4 {
		t.Fatalf("cpus: got %d, want 4", got.CPUs)
	}
	if got.NetworkMode != "shared" {
		t.Fatalf("network mode: got %q, want %q", got.NetworkMode, "shared")
	}
}

// promptDiskName re-prompts on a rejected name, so it reads twice from the
// reader it was given and must not lose the second line either.
func TestPromptDiskNameRePromptsFromPipedStdin(t *testing.T) {
	var stdout bytes.Buffer
	taken := map[string]struct{}{"kairos-disk0": {}}

	got, err := promptDiskName("kairos-disk0", taken, strings.NewReader("kairos-disk0\nkairos-disk1\n"), &stdout)
	if err != nil {
		t.Fatalf("promptDiskName on piped stdin: %v", err)
	}
	if got != "kairos-disk1" {
		t.Fatalf("got %q, want %q", got, "kairos-disk1")
	}
	if !strings.Contains(stdout.String(), "already exists") {
		t.Fatalf("expected the taken-name rejection to be printed, got:\n%s", stdout.String())
	}
}

// The end-to-end shape of kairos-io/kairos#5057: the whole command driven the
// way a script drives it, `printf '7\nshared\n\n' | kairos-lab start ...`,
// with stdin handing back more than one line per Read. Before Run buffered
// stdin once, this failed at the network-mode prompt with "no input" and the
// pre-flight was never reached.
func TestStartTakesTheReviewAnswersFromPipedStdin(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")

	refused := errors.New("this host cannot privilege the mode")
	calls := stubNetworkPrivilege(t, func(string) error { return refused })

	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-name", "kairos-disk0", "-no-iso", "-network", "user"},
		strings.NewReader("7\nshared\n\n"), &stdout, &stderr, "test")

	if !errors.Is(err, refused) {
		t.Fatalf("start returned %v, want the pre-flight's own refusal; stdout:\n%s", err, stdout.String())
	}
	if want := []string{"shared"}; !slices.Equal(*calls, want) {
		t.Fatalf("the pre-flight was asked %q, want %q", *calls, want)
	}
}

// The three prompts of the "create a new disk" path live in two packages:
// selectOrCreateDisk asks which disk, iso.SelectDownloaded asks which image,
// and promptDiskName asks for the name. Each used to buffer stdin for itself,
// so the disk picker swallowed the two answers meant for the other two. This
// is the cross-package half of kairos-io/kairos#5057.
func TestSelectOrCreateDiskCarriesPipedStdinAcrossPackages(t *testing.T) {
	store := &state.Store{CacheDir: t.TempDir(), ConfigDir: t.TempDir()}
	st := state.NewState(store)
	st.Disks = append(st.Disks, state.Disk{Name: "kairos-disk0", Path: "/tmp/kairos-disk0.qcow2", Size: "60G"})

	// Two ISOs, so the image picker asks rather than taking the only one.
	downloads := t.TempDir()
	for _, name := range []string{"kairos-a.iso", "kairos-b.iso"} {
		if err := os.WriteFile(filepath.Join(downloads, name), []byte("iso"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var stdout bytes.Buffer
	// n -> create new, 2 -> the second ISO, then the disk name.
	disk, isoLocal, isNew, err := selectOrCreateDisk(st, t.TempDir(), downloads, "60G",
		strings.NewReader("n\n2\nkairos-disk1\n"), &stdout)
	if err != nil {
		t.Fatalf("selectOrCreateDisk on piped stdin: %v\nstdout:\n%s", err, stdout.String())
	}
	if !isNew {
		t.Fatal("expected a new disk")
	}
	if disk.Name != "kairos-disk1" {
		t.Fatalf("disk name: got %q, want %q", disk.Name, "kairos-disk1")
	}
	if filepath.Base(isoLocal) != "kairos-b.iso" {
		t.Fatalf("iso: got %q, want kairos-b.iso", isoLocal)
	}
}

// Threading one reader through the prompts only works because the nested
// bufio.NewReader calls are no-ops on a reader that is already buffered.
// That is bufio.NewReaderSize's documented reuse, and it is load-bearing
// here: if it ever stopped holding, every prompt below Run would go back to
// owning a buffer and kairos-io/kairos#5057 would return. Pin it.
func TestBufioNewReaderReusesAnAlreadyBufferedReader(t *testing.T) {
	first := bufio.NewReader(strings.NewReader("one\ntwo\n"))
	if second := bufio.NewReader(first); second != first {
		t.Fatalf("bufio.NewReader wrapped a *bufio.Reader again: got %p, want %p", second, first)
	}
}

// Two prompting functions in a row, which is what makes the single wrap in
// Run load-bearing rather than a tidy-up: runStart calls selectOrCreateDisk
// to pick the disk and reviewVMConfig to settle the config, and each of them
// buffers what it is handed. Only Run buffering the process stdin first makes
// those two share a reader, so the disk picker cannot swallow the review's
// answers on the way past.
func TestStartSharesOneStdinBetweenTheDiskPickerAndTheReview(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	isolateFromHostBinaries(t)
	seedStartableState(t, "kairos-disk0")

	refused := errors.New("this host cannot privilege the mode")
	calls := stubNetworkPrivilege(t, func(string) error { return refused })

	// 1 -> the seeded disk, then 7 -> shared at the review, then leave.
	var stdout, stderr bytes.Buffer
	err := Run([]string{"start", "-no-iso", "-network", "user"},
		strings.NewReader("1\n7\nshared\n\n"), &stdout, &stderr, "test")

	if !errors.Is(err, refused) {
		t.Fatalf("start returned %v, want the pre-flight's own refusal; stdout:\n%s", err, stdout.String())
	}
	if want := []string{"shared"}; !slices.Equal(*calls, want) {
		t.Fatalf("the pre-flight was asked %q, want %q", *calls, want)
	}
}

// start polls for the guest's address for DefaultIPPollTimeout and records
// what it finds. A guest that accepts its lease after that window -- an
// installer ISO left at its first prompt is the ordinary case -- is never
// polled again, so state.json keeps no address for it. status used to print
// that recorded value alone, which meant a VM whose address the host's own
// lease file and ARP cache had held for minutes still reported "none" for
// the rest of its life.
func TestStatusResolvesTheAddressOfARunningVMThatHasNoneRecorded(t *testing.T) {
	var asked vm.IPLookup
	restore := resolveVMIP
	resolveVMIP = func(_ context.Context, l vm.IPLookup) (vm.IPResult, bool) {
		asked = l
		return vm.IPResult{IP: "192.168.2.5", Source: vm.IPSourceDHCPLease}, true
	}
	t.Cleanup(func() { resolveVMIP = restore })

	out := runStatusOutput(t, func(st *state.State) {
		st.Network.Mode = "shared"
		st.Disks = append(st.Disks, state.Disk{Name: testVMName, MAC: "52:54:00:6e:f0:c4"})
		withVMField(testVMName, func(v *state.VM) {
			v.NetworkMode = "shared"
			v.DiskName = testVMName
			v.PID = os.Getpid()
		})(st)
	})

	if !strings.Contains(out, "  vm ip address: 192.168.2.5\n") {
		t.Errorf("status did not report the address the host knows:\n%s", out)
	}
	if asked.MAC != "52:54:00:6e:f0:c4" {
		t.Errorf("status asked about MAC %q, want the disk's recorded one", asked.MAC)
	}
	if asked.Mode != "shared" {
		t.Errorf("status asked in mode %q, want shared", asked.Mode)
	}
}

// The recorded address always wins. It is what the start that watched this
// guest resolved, and re-asking the host could answer from an ARP entry left
// by an earlier boot of the same deterministic MAC.
func TestStatusPrefersTheRecordedAddressOverALiveLookup(t *testing.T) {
	restore := resolveVMIP
	resolveVMIP = func(context.Context, vm.IPLookup) (vm.IPResult, bool) {
		t.Error("status asked the host although the address was recorded")
		return vm.IPResult{IP: "10.0.0.1"}, true
	}
	t.Cleanup(func() { resolveVMIP = restore })

	out := runStatusOutput(t, func(st *state.State) {
		st.Network.Mode = "shared"
		withVMField(testVMName, func(v *state.VM) {
			v.NetworkMode = "shared"
			v.IPAddress = "192.168.64.12"
			v.PID = os.Getpid()
		})(st)
	})
	if !strings.Contains(out, "  vm ip address: 192.168.64.12\n") {
		t.Errorf("status did not print the recorded address:\n%s", out)
	}
}

// A VM that is not running has no address to ask about: whatever the host
// still holds for that MAC is a previous boot's, and MACForDisk is
// deterministic, so the stale entry would match and be reported as current.
func TestStatusDoesNotAskTheHostAboutAStoppedVM(t *testing.T) {
	restore := resolveVMIP
	resolveVMIP = func(context.Context, vm.IPLookup) (vm.IPResult, bool) {
		t.Error("status asked the host about a VM that is not running")
		return vm.IPResult{IP: "10.0.0.1"}, true
	}
	t.Cleanup(func() { resolveVMIP = restore })

	out := runStatusOutput(t, func(st *state.State) {
		st.Network.Mode = "shared"
		withVMField(testVMName, func(v *state.VM) { v.NetworkMode = "shared" })(st)
	})
	if !strings.Contains(out, "  vm ip address: none\n") {
		t.Errorf("status did not report a stopped VM's address as none:\n%s", out)
	}
}

// user mode has no host source at all: the DHCP server is inside QEMU and no
// frame reaches the host's neighbour table. Asking would spend the timeout
// to learn nothing, and the forwards row below is what that mode has.
func TestStatusDoesNotAskTheHostInUserMode(t *testing.T) {
	restore := resolveVMIP
	resolveVMIP = func(context.Context, vm.IPLookup) (vm.IPResult, bool) {
		t.Error("status asked the host in user mode, which has no host source")
		return vm.IPResult{IP: "10.0.2.15"}, true
	}
	t.Cleanup(func() { resolveVMIP = restore })

	out := runStatusOutput(t, func(st *state.State) {
		st.Network.Mode = "user"
		withVMField(testVMName, func(v *state.VM) {
			v.NetworkMode = "user"
			v.PID = os.Getpid()
		})(st)
	})
	if !strings.Contains(out, "  vm ip address: none\n") {
		t.Errorf("status did not report user mode's address as none:\n%s", out)
	}
}
