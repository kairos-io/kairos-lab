package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadMissingReturnsDefault(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != SchemaVersion {
		t.Fatalf("unexpected version: %d", st.Version)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(store)
	st.Platform.OS = "linux"
	st.Setup.InstalledByKairosLab = []string{"qemu"}
	stateFile := filepath.Join(store.ConfigDir, "state.json")
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Platform.OS != "linux" {
		t.Fatalf("platform mismatch: %s", loaded.Platform.OS)
	}
	if len(loaded.Setup.InstalledByKairosLab) != 1 || loaded.Setup.InstalledByKairosLab[0] != "qemu" {
		t.Fatalf("dependencies mismatch: %#v", loaded.Setup.InstalledByKairosLab)
	}
}

func TestSaveLoadRoundTripKeepsMACAndIP(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(store)
	AddDisk(st, Disk{
		Name:      "kairos-core-20250101-120000",
		Path:      "/tmp/kairos.qcow2",
		CreatedAt: "2025-01-01T12:00:00Z",
		Size:      "60G",
		MAC:       "52:54:00:ab:cd:ef",
	})
	st.VM.IPAddress = "192.168.64.7"
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	disk := FindDiskByName(loaded, "kairos-core-20250101-120000")
	if disk == nil {
		t.Fatal("disk missing after reload")
	}
	if disk.MAC != "52:54:00:ab:cd:ef" {
		t.Errorf("disk MAC = %q, want 52:54:00:ab:cd:ef", disk.MAC)
	}
	if loaded.VM.IPAddress != "192.168.64.7" {
		t.Errorf("vm IP address = %q, want 192.168.64.7", loaded.VM.IPAddress)
	}
}

func TestLoadStateWrittenBeforeMACField(t *testing.T) {
	// A disk recorded before the mac field existed has no such key at all. It
	// must load without error and simply arrive with an empty MAC, which the
	// start path will fill in once it is wired up to do so; there is no
	// migration and no schema bump.
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"disks":[{"name":"old","path":"/tmp/old.qcow2","created_at":"2025-01-01T00:00:00Z","size":"60G"}]}`
	if err := os.WriteFile(filepath.Join(store.ConfigDir, "state.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("loading a pre-MAC state file should succeed: %v", err)
	}
	disk := FindDiskByName(st, "old")
	if disk == nil {
		t.Fatal("disk \"old\" missing from the loaded state")
	}
	if disk.MAC != "" {
		t.Errorf("disk MAC = %q, want empty for a pre-MAC state file", disk.MAC)
	}
	if st.VM.IPAddress != "" {
		t.Errorf("vm IP address = %q, want empty for a pre-MAC state file", st.VM.IPAddress)
	}
}

// TestStateJSONKeysForMACAndIP pins the on-disk JSON contract for the two
// fields added with the per-VM MAC work: the key names themselves, and the
// omitempty that keeps them out of a file where they carry no value. Renaming
// either key, or dropping omitempty from either, leaves every round-trip test
// green because those only ever go through this package's own structs -- so
// this test reads the raw bytes instead.
func TestStateJSONKeysForMACAndIP(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}

	st := NewState(store)
	AddDisk(st, Disk{
		Name:      "kairos-core-20250101-120000",
		Path:      "/tmp/kairos.qcow2",
		CreatedAt: "2025-01-01T12:00:00Z",
		Size:      "60G",
	})
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mac"`, `"ip_address"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("state.json should omit %s when the value is empty, got:\n%s", key, raw)
		}
	}

	st.Disks[0].MAC = "52:54:00:ab:cd:ef"
	st.VM.IPAddress = "192.168.64.7"
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(store.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mac"`, `"ip_address"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("state.json should carry the %s key once it has a value, got:\n%s", key, raw)
		}
	}
}

// blockedStore returns a store that shares cfg's directories but whose state
// path is an existing directory. os.Rename can never take over a directory
// name, so a Save through the returned store is guaranteed to fail at exactly
// the point this package cares about -- after the temporary file exists --
// without mocking the filesystem out from under it.
func blockedStore(t *testing.T, cfg *Store) *Store {
	t.Helper()
	blocked := filepath.Join(cfg.ConfigDir, "blocked-state.json")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Store{ConfigDir: cfg.ConfigDir, CacheDir: cfg.CacheDir, StatePath: blocked}
}

// sealDir makes dir unwritable for the rest of the test, so that anything
// trying to create a new entry in it -- the temporary file Save opens, in
// particular -- fails with EACCES before it has touched a single existing
// file. The restore exists because t.TempDir's own cleanup cannot remove a
// tree it is not allowed to write to and would fail the test on the way out.
// Registering it before the chmod rather than after is defensive habit and not
// the thing that makes that work: t.Cleanup runs its functions last registered
// first, and t.TempDir registered its teardown back when it created the
// directory, so a restore registered on either side of the chmod still runs
// before that teardown either way.
func sealDir(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
}

// TestSaveFailureLeavesPreviousStateIntact is the whole point of writing
// through a rename: a save that cannot complete must leave the file that was
// already on disk byte-for-byte as it was, still parseable by the next reader,
// rather than the truncated stump a failed in-place write would leave behind.
//
// The failure is induced by sealing the directory the state file lives in,
// which is what makes this test discriminate rather than decorate. An in-place
// os.WriteFile does not need write permission on the directory to reopen a
// file that already exists and is writable by its owner -- it would truncate
// the live state.json and then succeed -- whereas creating the temporary file
// has to add an entry to the directory and fails before writing anything.
// Pointing the failure at some other path than the one the assertions read
// would let the old implementation pass this test unchanged.
func TestSaveFailureLeavesPreviousStateIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission, so the save under test would succeed")
	}
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(store)
	st.Platform.OS = "linux"
	st.VM.IPAddress = "192.168.64.7"
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.StatePath)
	if err != nil {
		t.Fatal(err)
	}

	sealDir(t, filepath.Dir(store.StatePath))
	st.Platform.OS = "darwin"
	st.VM.IPAddress = "10.0.0.1"
	if err := store.Save(st); err == nil {
		t.Fatal("saving into a directory that cannot be written should fail")
	}

	after, err := os.ReadFile(store.StatePath)
	if err != nil {
		t.Fatalf("the previous state file should still be readable: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a failed save rewrote the previous state file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("the previous state file should still parse: %v", err)
	}
	if loaded.Platform.OS != "linux" || loaded.VM.IPAddress != "192.168.64.7" {
		t.Errorf("previous state changed: os = %q, ip = %q", loaded.Platform.OS, loaded.VM.IPAddress)
	}
}

// TestSaveFailureLeavesNoTemporaryFile pins the cleanup half of the rename
// dance. The temporary file is created beside the state file -- the config dir,
// for the store this test builds -- so forgetting to remove it on failure would
// slowly fill a directory the user actually looks at with state.json.tmp-*
// debris. Three failing saves rather than one, so a leak shows up as debris
// accumulating rather than a single file that could be mistaken for the real
// one.
func TestSaveFailureLeavesNoTemporaryFile(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(NewState(store)); err != nil {
		t.Fatal(err)
	}

	doomed := blockedStore(t, store)
	for i := 0; i < 3; i++ {
		if err := doomed.Save(NewState(store)); err == nil {
			t.Fatal("saving onto a directory should fail")
		}
	}

	entries, err := os.ReadDir(store.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"blocked-state.json", "state.json"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("config dir contents = %v, want %v", names, want)
	}
}

// TestSaveUsesTheStatePathDirectoryForItsTemporaryFile pins which directory the
// temporary file is created in, for a Store whose StatePath is not inside its
// ConfigDir. Store is exported with exported fields and nothing makes those two
// agree, and only the directory StatePath lives in is guaranteed to be on the
// same filesystem as the rename's destination -- across two filesystems rename
// fails outright with EXDEV rather than copying.
//
// Every other test in this package builds its store through DefaultStore, which
// always puts state.json inside ConfigDir, so all of them stay green if the
// create is pointed back at ConfigDir. This one is the discriminator, and it
// discriminates twice over so that it does so as any user:
//
//   - ConfigDir is sealed, so a create attempted there fails with EACCES and
//     takes the whole save down with it. Root ignores directory permissions, so
//     that half only runs when the tests are not root.
//   - ConfigDir's modification time is compared across the save. A directory's
//     mtime moves when an entry is added to it, and moves again when one is
//     removed, so a temporary file created there and renamed away leaves the
//     evidence behind even though the file itself is gone. Nothing in a correct
//     save touches that directory at all: MkdirAll finds it already there and
//     returns without writing.
func TestSaveUsesTheStatePathDirectoryForItsTemporaryFile(t *testing.T) {
	configDir := t.TempDir()
	stateDir := t.TempDir()
	store := &Store{
		ConfigDir: configDir,
		CacheDir:  t.TempDir(),
		StatePath: filepath.Join(stateDir, "state.json"),
	}
	if os.Geteuid() != 0 {
		sealDir(t, configDir)
	}
	before, err := os.Stat(configDir)
	if err != nil {
		t.Fatal(err)
	}

	st := NewState(store)
	st.Platform.Arch = "arm64"
	if err := store.Save(st); err != nil {
		t.Fatalf("a state path outside ConfigDir should still save: %v", err)
	}

	if _, err := os.Stat(store.StatePath); err != nil {
		t.Fatalf("the state file should exist at StatePath: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("the saved state file should parse: %v", err)
	}
	if loaded.Platform.Arch != "arm64" {
		t.Errorf("arch = %q, want arm64", loaded.Platform.Arch)
	}

	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("ConfigDir contents after the save = %v, want nothing: the state file and its temporary belong beside StatePath", names)
	}
	after, err := os.Stat(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("ConfigDir was modified by a save that should not have touched it (mtime %v -> %v): the temporary file was created there instead of beside StatePath",
			before.ModTime(), after.ModTime())
	}
}

// TestSavePreservesExistingStateFileMode is the mode half of writing through a
// rename. An in-place write left the mode of an existing file untouched --
// O_CREATE|O_TRUNC ignores its mode argument once the file exists -- so a user
// who tightened state.json by hand kept it tightened across every later save.
// A rename publishes the temporary file's own mode instead, which would undo
// that silently on the next save, so Save has to copy the old mode onto the
// replacement. The 0644 case is here because carrying the mode across has to
// mean carrying it, not clamping it: a state.json that is already group- and
// world-readable stays exactly that.
func TestSavePreservesExistingStateFileMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o644} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
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
			// chmod rather than a mode argument, because os.WriteFile and friends
			// run their mode through the umask and would not reliably produce the
			// mode this case is about.
			if err := os.Chmod(store.StatePath, mode); err != nil {
				t.Fatal(err)
			}

			st.VM.IPAddress = "192.168.64.7"
			if err := store.Save(st); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(store.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != mode {
				t.Errorf("state file mode after a save = %04o, want %04o", perm, mode)
			}
			loaded, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.VM.IPAddress != "192.168.64.7" {
				t.Errorf("vm IP address = %q, want 192.168.64.7", loaded.VM.IPAddress)
			}
		})
	}
}

// TestConcurrentSaveAndLoad reproduces the situation the rename exists for: a
// reader (`kairos-lab status` in another terminal) hitting state.json while a
// writer is replacing it. Every Load must return a fully parsed state -- never
// a parse error from a truncated file. The saved payload changes length from
// iteration to iteration so that a partially written file would be visibly
// malformed rather than accidentally valid JSON.
func TestConcurrentSaveAndLoad(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(NewState(store)); err != nil {
		t.Fatal(err)
	}

	const saves = 50
	const loads = 200
	// One slot each: both goroutines return after their first send, so a
	// capacity matching the loop count would only ever hold one value anyway.
	saveErrs := make(chan error, 1)
	loadErrs := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < saves; i++ {
			st := NewState(store)
			st.VM.LastError = strings.Repeat("x", i*8)
			if err := store.Save(st); err != nil {
				saveErrs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < loads; i++ {
			if _, err := store.Load(); err != nil {
				loadErrs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(saveErrs)
	close(loadErrs)

	for err := range saveErrs {
		t.Errorf("concurrent save failed: %v", err)
	}
	for err := range loadErrs {
		t.Errorf("concurrent load could not read the state file: %v", err)
	}
}
