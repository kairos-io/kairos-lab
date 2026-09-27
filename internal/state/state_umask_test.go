//go:build unix

package state

// The tests in this file pin file modes, so they need the process umask to be
// a known value rather than whatever the developer's shell or the CI runner
// happened to set, and syscall.Umask is how they get one.
//
// The unix constraint is what guarantees that call exists: Windows and plan9
// have no umask at all. It also excludes js/wasm and wasip1, which do have
// one -- neither is a target this repo builds, and dropping two tests on a
// platform where they would have worked is the conservative direction of the
// two.
//
// The rest of the package's tests need none of this and stay in
// state_test.go, unconstrained, so a build for a platform without a umask
// loses these and keeps those. Splitting rather than constraining the whole
// test file is the point: a file-wide tag would have taken every portable
// test in the package with it.

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// pinUmask fixes the process umask for the duration of one test and puts the
// previous value back afterwards, so a test about file modes measures the
// value it names rather than whatever the developer's shell or the CI runner
// happened to set -- neither of which is knowable from here, and neither of
// which is the umask any single case below is about.
//
// The umask is a property of the whole process and not of the goroutine that
// sets it, so this is only safe because nothing in this package calls
// t.Parallel -- Go runs the tests of one package sequentially otherwise.
// syscall.Umask is also the reason for the build constraint at the top of
// this file.
func pinUmask(t *testing.T, mask int) {
	t.Helper()
	previous := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(previous) })
}

// TestSaveCreatesStateFileHonouringUmask pins the mode a state.json gets when
// there is none yet: the 0644 the create asks for, with the process umask
// subtracted from it by the kernel. That is exactly what the in-place
// os.WriteFile this save path replaced produced, and reproducing it is the
// point -- publishing by rename is a durability change and should not quietly
// become a policy change about who may read the file.
//
// This test used to assert 0600, the mode os.CreateTemp hardcodes, and that
// assertion was itself the defect. Running the whole tool under sudo is
// blessed on macOS -- vm.checkDarwinPrivilege accepts euid 0 and its refusal
// says to re-run with sudo -- and the stock sudoers keeps HOME, so
// `sudo kairos-lab setup` resolves os.UserConfigDir to the invoking user's
// config dir and creates state.json there owned by root. At 0644 that file is
// still readable by the user afterwards. At 0600 it is not, and Store.Load
// falls back only for a file that is absent, never for one it may not open, so
// every later unprivileged command returns that error -- including `reset` and
// `cleanup`, the two a user would reach for to get out of it.
//
// Both directions are covered, because a mode handed to a create is a ceiling
// and not a floor: a permissive umask must not widen the file past 0644, and a
// restrictive one must still be obeyed rather than overridden.
func TestSaveCreatesStateFileHonouringUmask(t *testing.T) {
	for _, tc := range []struct {
		umask int
		want  os.FileMode
	}{
		{umask: 0o022, want: 0o644},
		{umask: 0o077, want: 0o600},
	} {
		t.Run(fmt.Sprintf("umask%03o", tc.umask), func(t *testing.T) {
			pinUmask(t, tc.umask)
			t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
			t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
			store, err := DefaultStore()
			if err != nil {
				t.Fatal(err)
			}
			st := NewState(store)
			st.Platform.Arch = "arm64"
			if err := store.Save(st); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Platform.Arch != "arm64" {
				t.Errorf("arch = %q, want arm64", loaded.Platform.Arch)
			}
			info, err := os.Stat(store.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != tc.want {
				t.Errorf("mode of a freshly created state file under umask %03o = %04o, want %04o", tc.umask, perm, tc.want)
			}
		})
	}
}

// TestSaveReplacesASymlinkAtStatePath covers what the rename does when a
// symlink is sitting where state.json should be, and the two modes Save must
// not take from it.
//
// The rename replaces the link itself and leaves the file it pointed at
// untouched. That is a deliberate improvement on the in-place write this save
// path replaced, which followed the link and wrote through it: a link planted
// at a path this tool writes -- and on macOS it may be writing as root, into a
// directory the invoking user owns -- turned a save into a write to whatever
// the planter chose.
//
// The mode assertions are the other half of the same point. Since the link is
// replaced rather than written through, the mode of its target is the mode of
// nothing this function publishes, so it must not be carried onto the new
// file: os.Stat would hand it over, because it follows the link, which is why
// the carry uses os.Lstat. Nor may the link's own mode be carried -- 0777 on
// Linux, 0755 on macOS, neither of them anything a state file should be --
// which is what the IsRegular guard beside the Lstat is for. Either mistake
// surfaces here as a published mode that is not the 0644-minus-umask a create
// gives a file that did not exist.
func TestSaveReplacesASymlinkAtStatePath(t *testing.T) {
	pinUmask(t, 0o022)
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// chmod rather than a mode argument, so the target's mode is the one this
	// test names and not that mode minus whatever umask is in force.
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.StatePath); err != nil {
		t.Fatal(err)
	}

	st := NewState(store)
	st.Platform.Arch = "arm64"
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(store.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the symlink at StatePath should have been replaced by the state file, not followed")
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("published state file mode = %04o, want 0644: neither a symlink's mode nor its target's is the mode of a file being created", perm)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Platform.Arch != "arm64" {
		t.Errorf("arch = %q, want arm64", loaded.Platform.Arch)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "target\n" {
		t.Errorf("the symlink target was written through: contents = %q, want it untouched", body)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := targetInfo.Mode().Perm(); perm != 0o640 {
		t.Errorf("symlink target mode = %04o, want 0640 untouched", perm)
	}
}
