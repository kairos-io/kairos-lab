package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeRawState(t *testing.T, store *Store, raw string) {
	t.Helper()
	if err := os.MkdirAll(store.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatePath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateLegacyVMWithDiskName is the ordinary v1->v2 case: a legacy "vm"
// record carrying disk_name folds into VMs keyed on that name, at index 0,
// with schema version re-stamped to 2.
func TestMigrateLegacyVMWithDiskName(t *testing.T) {
	store := newTestStore(t)
	legacy := `{"version":1,"vm":{"disk_name":"kairos-core","disk_path":"/tmp/kairos-core.qcow2","pid":1234,"ip_address":"192.168.64.7"}}`
	writeRawState(t, store, legacy)

	st, err := store.Load()
	if err != nil {
		t.Fatalf("loading a v1 file should succeed: %v", err)
	}
	if st.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", st.Version, SchemaVersion)
	}
	if st.Legacy != nil {
		t.Errorf("legacy field should be nil after migration, got %+v", st.Legacy)
	}
	vm := FindVM(st, "kairos-core")
	if vm == nil {
		t.Fatalf("migrated VM missing, have %#v", st.VMs)
	}
	if vm.Index != 0 {
		t.Errorf("migrated VM index = %d, want 0", vm.Index)
	}
	if vm.PID != 1234 || vm.IPAddress != "192.168.64.7" {
		t.Errorf("migrated VM fields not carried over: %+v", vm)
	}
	if len(st.VMs) != 1 {
		t.Errorf("VMs = %#v, want exactly one entry", st.VMs)
	}
}

// TestMigrateLegacyVMFallsBackToDiskPathBasename covers a file recorded
// before disk_name existed: the VM's name falls back to the base name of
// disk_path rather than being left empty.
func TestMigrateLegacyVMFallsBackToDiskPathBasename(t *testing.T) {
	store := newTestStore(t)
	legacy := `{"version":1,"vm":{"disk_path":"/home/user/.cache/kairos-lab/vm/kairos-core.qcow2","pid":42}}`
	writeRawState(t, store, legacy)

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	vm := FindVM(st, "kairos-core.qcow2")
	if vm == nil {
		t.Fatalf("expected a VM named after the disk path's basename, have %#v", st.VMs)
	}
}

// TestMigrateLegacyVMNeverStarted covers a v1 file whose "vm" key is present
// -- the old field had no omitempty, so every real v1 file carries one -- but
// entirely zero, because no VM had ever been started. It must still migrate
// without error, arriving with an empty Name (no disk_name, no disk_path to
// fall back to): that unnamed record is what keeps today's "any VM" refusal
// working for a config dir this old.
func TestMigrateLegacyVMNeverStarted(t *testing.T) {
	store := newTestStore(t)
	legacy := `{"version":1,"vm":{}}`
	writeRawState(t, store, legacy)

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.VMs) != 1 {
		t.Fatalf("VMs = %#v, want exactly one (unnamed) entry", st.VMs)
	}
	if st.VMs[0].Name != "" {
		t.Errorf("name = %q, want empty for a VM that never started", st.VMs[0].Name)
	}
}

// TestMigrateLegacyVMNoVMKeyAtAll is the plan's other named case: a file with
// no "vm" key at all (Legacy unmarshals to nil, not to a zero VM). Nothing is
// migrated.
func TestMigrateLegacyVMNoVMKeyAtAll(t *testing.T) {
	store := newTestStore(t)
	writeRawState(t, store, `{"version":1,"disks":[{"name":"old","path":"/tmp/old.qcow2","created_at":"2025-01-01T00:00:00Z","size":"60G"}]}`)

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.VMs) != 0 {
		t.Errorf("VMs = %#v, want none", st.VMs)
	}
}

// TestMigrateLegacyVMToleratesNonConformingNetworkTapName pins that a stray,
// non-generated tap name left in the top-level network block by an old
// binary does not block migration or Load -- Network is carried through
// as-is, and it is internal/vm's job (M2/M3), not Load's, to decide whether a
// stored tap name is trustworthy.
func TestMigrateLegacyVMToleratesNonConformingNetworkTapName(t *testing.T) {
	store := newTestStore(t)
	legacy := `{"version":1,"network":{"mode":"shared","bridge_name":"kairoslab0","tap_name":"eth0"},"vm":{"disk_name":"kairos-core"}}`
	writeRawState(t, store, legacy)

	st, err := store.Load()
	if err != nil {
		t.Fatalf("a non-conforming network.tap_name should not fail Load: %v", err)
	}
	if st.Network.TapName != "eth0" {
		t.Errorf("network tap name = %q, want it carried through unchanged", st.Network.TapName)
	}
	if FindVM(st, "kairos-core") == nil {
		t.Fatal("migration should still have run")
	}
}

// TestMigrationIsIdempotent: Load is called by status and may never be
// followed by a Save, so a second Load of the same still-v1 file on disk
// must not duplicate the migrated record, and a Load-Save-Load round trip
// must not either, now that Legacy is nil and omitted from what was written.
func TestMigrationIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	writeRawState(t, store, `{"version":1,"vm":{"disk_name":"kairos-core","pid":99}}`)

	first, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.VMs) != 1 || len(second.VMs) != 1 {
		t.Fatalf("two independent Loads of the same v1 file: got %d and %d VMs, want 1 and 1", len(first.VMs), len(second.VMs))
	}

	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.VMs) != 1 {
		t.Fatalf("after Load-Save-Load, VMs = %#v, want exactly one entry", reloaded.VMs)
	}
	raw, err := os.ReadFile(store.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContains(raw, `"vm"`) {
		t.Errorf("a saved v2 file should not carry the legacy \"vm\" key any more, got:\n%s", raw)
	}
}

func bytesContains(b []byte, s string) bool {
	return len(b) > 0 && (func() bool {
		for i := 0; i+len(s) <= len(b); i++ {
			if string(b[i:i+len(s)]) == s {
				return true
			}
		}
		return false
	})()
}

// TestLoadRejectsNewerSchemaVersion: an older binary reading a file a newer
// one wrote must refuse rather than silently drop whatever it does not
// understand.
func TestLoadRejectsNewerSchemaVersion(t *testing.T) {
	store := newTestStore(t)
	writeRawState(t, store, fmt.Sprintf(`{"version":%d}`, SchemaVersion+1))

	if _, err := store.Load(); err == nil {
		t.Fatal("loading a file with a newer schema version should fail")
	}
}

// TestLoadReStampsVersionUnconditionally: Load used to fix up only
// Version == 0; it now re-stamps to SchemaVersion on every successful read,
// and a Save afterwards persists that.
func TestLoadReStampsVersionUnconditionally(t *testing.T) {
	store := newTestStore(t)
	writeRawState(t, store, `{"version":1,"vm":{"disk_name":"kairos-core"}}`)

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != SchemaVersion {
		t.Fatalf("version = %d, want %d", st.Version, SchemaVersion)
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`"version": %d`, SchemaVersion)
	if !bytesContains(raw, want) {
		t.Errorf("saved file does not carry %q, got:\n%s", want, raw)
	}
}

// TestQuarantinedRecordLeavesStatusWorking: a state.json carrying one VM
// record this binary cannot trust (an out-of-range index) must not fail the
// whole Load. The bad record is set aside in Quarantined and the good one is
// still usable.
func TestQuarantinedRecordLeavesStatusWorking(t *testing.T) {
	store := newTestStore(t)
	raw := `{"version":2,"vms":[` +
		`{"name":"good-vm","index":1},` +
		`{"name":"bad-vm","index":500}` +
		`]}`
	writeRawState(t, store, raw)

	st, err := store.Load()
	if err != nil {
		t.Fatalf("a quarantined record should not fail Load: %v", err)
	}
	if FindVM(st, "good-vm") == nil {
		t.Error("good-vm should still be usable")
	}
	if FindVM(st, "bad-vm") != nil {
		t.Error("bad-vm has an out-of-range index and should have been quarantined, not kept")
	}
	if len(st.Quarantined) != 1 || st.Quarantined[0].Name != "bad-vm" {
		t.Errorf("Quarantined = %#v, want exactly one entry naming bad-vm", st.Quarantined)
	}
}

// TestQuarantinedRecordRejectsBadNetworkMode covers the other validated
// field, invalidVMReason's NetworkMode check: a network mode this binary
// does not recognise.
func TestQuarantinedRecordRejectsBadNetworkMode(t *testing.T) {
	store := newTestStore(t)
	writeRawState(t, store, `{"version":2,"vms":[{"name":"weird","network_mode":"carrier-pigeon"}]}`)

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if FindVM(st, "weird") != nil {
		t.Error("a VM with an unknown network mode should have been quarantined")
	}
	if len(st.Quarantined) != 1 {
		t.Fatalf("Quarantined = %#v, want exactly one entry", st.Quarantined)
	}
}

// TestQuarantineToleratesEmptyName: an empty Name is not itself a quarantine
// reason -- it is what a pre-DiskName v1 file with no VM ever started
// migrates to, and it carries no path or argv anywhere, so it is not
// adversarial the way a malformed non-empty name is.
func TestQuarantineToleratesEmptyName(t *testing.T) {
	store := newTestStore(t)
	writeRawState(t, store, `{"version":2,"vms":[{"index":0}]}`)

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Quarantined) != 0 {
		t.Errorf("an unnamed record should not be quarantined, got %#v", st.Quarantined)
	}
	if len(st.VMs) != 1 {
		t.Errorf("VMs = %#v, want the unnamed record kept", st.VMs)
	}
}

// TestUpdateSerialisesConcurrentGoroutines is the seam-gate test for
// Update's locking: two goroutines both calling Update on the same Store
// must never lose a write, which is only a real test when each Update opens
// a fresh file descriptor -- two descriptors from one open() call in one
// process would share an open file description and never contend at all.
// Run under -race.
func TestUpdateSerialisesConcurrentGoroutines(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(NewState(store)); err != nil {
		t.Fatal(err)
	}

	const perGoroutine = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for g := 0; g < 2; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				name := fmt.Sprintf("vm-%d-%d", g, i)
				err := store.Update(func(st *State) error {
					UpsertVM(st, VM{Name: name, PID: i + 1})
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Update failed: %v", err)
	}

	final, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(final.VMs) != 2*perGoroutine {
		t.Errorf("VMs after %d concurrent Updates = %d, want %d (no write should have been lost)",
			2*perGoroutine, len(final.VMs), 2*perGoroutine)
	}
}

// TestUpdateFnErrorLeavesStateUnsaved: a validation failure inside fn must
// not publish a half-updated state.json.
func TestUpdateFnErrorLeavesStateUnsaved(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(NewState(store)); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("boom")
	err := store.Update(func(st *State) error {
		UpsertVM(st, VM{Name: "should-not-persist"})
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want %v", err, wantErr)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if FindVM(st, "should-not-persist") != nil {
		t.Error("a state change made by a fn that returned an error should not have been saved")
	}
}

// TestUpdateReentrantCallReturnsAnError: Update called again, on the same
// Store, from inside its own fn (same goroutine, still holding the lock)
// must return an error rather than hang. The timeout is shrunk so the test
// does not cost the production 30s budget.
func TestUpdateReentrantCallReturnsAnError(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(NewState(store)); err != nil {
		t.Fatal(err)
	}

	oldTimeout, oldInterval := lockAcquireTimeout, lockRetryInterval
	lockAcquireTimeout = 300 * time.Millisecond
	lockRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { lockAcquireTimeout, lockRetryInterval = oldTimeout, oldInterval })

	err := store.Update(func(st *State) error {
		return store.Update(func(inner *State) error {
			return nil
		})
	})
	if err == nil {
		t.Fatal("a re-entrant Update should return an error, not succeed")
	}
}

// TestSaveUsesTheStatePathDirectoryForItsLockFile pins that Update's lock
// file, like Save's temporary file, lives beside StatePath and not inside
// ConfigDir when the two differ -- Update keys the lock off
// filepath.Dir(s.StatePath), not ConfigDir, since nothing makes the two
// agree.
func TestUpdateLockFileLivesBesideStatePath(t *testing.T) {
	configDir := t.TempDir()
	stateDir := t.TempDir()
	store := &Store{
		ConfigDir: configDir,
		CacheDir:  t.TempDir(),
		StatePath: filepath.Join(stateDir, "state.json"),
	}
	if err := store.Save(NewState(store)); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, ".state.lock")); err != nil {
		t.Errorf("expected a lock file beside StatePath: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, ".state.lock")); err == nil {
		t.Errorf("lock file should not have been created inside ConfigDir")
	}
}
