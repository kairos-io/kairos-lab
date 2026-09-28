package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const SchemaVersion = 2

// MaxVMIndex is the highest per-VM index this schema allocates. It bounds
// both NextFreeVMIndex and the validation Load applies to a stored index: an
// index above it is not a name TapNameForIndex or TapConnNameForIndex would
// ever produce, so a record carrying one is quarantined rather than trusted.
const MaxVMIndex = 99

type Platform struct {
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	PackageManager string `json:"package_manager"`
}

type Setup struct {
	CompletedAt           string   `json:"completed_at,omitempty"`
	PreExistingDeps       []string `json:"pre_existing_deps,omitempty"`
	InstalledByKairosLab  []string `json:"installed_by_kairos_lab,omitempty"`
	DependencyCheckPassed bool     `json:"dependency_check_passed"`
}

type Network struct {
	Mode                 string   `json:"mode,omitempty"`
	BridgeInterface      string   `json:"bridge_interface,omitempty"`
	BridgeName           string   `json:"bridge_name,omitempty"`
	TapName              string   `json:"tap_name,omitempty"`
	DHCPPIDFile          string   `json:"dhcp_pid_file,omitempty"`
	DHCPLeaseFile        string   `json:"dhcp_lease_file,omitempty"`
	CreatedByKairosLab   bool     `json:"created_by_kairos_lab"`
	CreatedResources     []string `json:"created_resources,omitempty"`
	CleanupRequired      bool     `json:"cleanup_required"`
	LastPreparedAt       string   `json:"last_prepared_at,omitempty"`
	LastCleanupAttemptAt string   `json:"last_cleanup_attempt_at,omitempty"`
}

type Disk struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	ISOName   string `json:"iso_name,omitempty"`
	CreatedAt string `json:"created_at"`
	Size      string `json:"size"`
	MemoryGB  int    `json:"memory_gb,omitempty"`
	CPUs      int    `json:"cpus,omitempty"`
	// MAC is the per-disk QEMU NIC address. It is additive and omitempty, so a
	// disk recorded before this field existed loads with an empty MAC; the
	// start path will fill one in once it is wired up to do so. No state
	// migration is needed either way.
	MAC string `json:"mac,omitempty"`
}

// VM is one VM's record. D1 identifies a VM by its disk name, so Name is the
// key every accessor below keys on; Index is the per-config-dir slot D6/D7
// allocate it, and everything below TapName is additive for multi-VM support
// (see the state package doc comment above Load for the migration that fills
// these in for a pre-multi-VM file).
type VM struct {
	Name        string   `json:"name,omitempty"`
	Index       int      `json:"index,omitempty"`
	ISOSource   string   `json:"iso_source,omitempty"`
	ISOInput    string   `json:"iso_input,omitempty"`
	ISOLocal    string   `json:"iso_local_path,omitempty"`
	DiskPath    string   `json:"disk_path,omitempty"`
	DiskName    string   `json:"disk_name,omitempty"`
	LogPath     string   `json:"log_path,omitempty"`
	QemuBinary  string   `json:"qemu_binary,omitempty"`
	QemuArgs    []string `json:"qemu_args,omitempty"`
	PID         int      `json:"pid,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	StoppedAt   string   `json:"stopped_at,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
	RuntimeDir  string   `json:"runtime_dir,omitempty"`
	QGASockPath string   `json:"qga_socket_path,omitempty"`
	IPAddress   string   `json:"ip_address,omitempty"`
	// TapName and TapConnName are the per-VM network identity D2/D7 give this
	// record. internal/vm derives both from Index rather than trusting these
	// once a VM starts; they are carried here so status has something to show
	// without recomputing the formula itself.
	TapName     string `json:"tap_name,omitempty"`
	TapConnName string `json:"tap_conn_name,omitempty"`
	NetworkMode string `json:"network_mode,omitempty"`
	SSHPort     int    `json:"ssh_port,omitempty"`
	HTTPPort    int    `json:"http_port,omitempty"`
	// StarterPID and StartingAt are D4's reservation half: written before
	// command.Start() so a second start racing the first sees this record as
	// live even before QEMU's own PID exists to check.
	StarterPID int    `json:"starter_pid,omitempty"`
	StartingAt string `json:"starting_at,omitempty"`
}

// QuarantinedVM names a VM record Load found in state.json but could not
// trust, and why. It is never itself persisted -- state.json's "vms" array
// carries only records that passed validation -- so a state file written by
// an older or buggy binary cannot brick status, reset or cleanup: the bad
// record is set aside instead of failing the whole file, and named here so
// the user can see it rather than have it silently vanish.
type QuarantinedVM struct {
	Name   string
	Reason string
}

type State struct {
	Version  int      `json:"version"`
	Platform Platform `json:"platform"`
	Setup    Setup    `json:"setup"`
	Network  Network  `json:"network"`
	VMs      []VM     `json:"vms,omitempty"`
	// Legacy is the pre-multi-VM "vm" record. It is a pointer and not a VM
	// value so Load can tell "absent from the file" (nil) from "present and
	// the zero value" (non-nil, pointing at a VM that never started) -- the
	// old field had no omitempty, so every real v1 file carries a "vm" key
	// even when no VM has ever run, and the migration in Load needs to fold
	// that in exactly once rather than mistake JSON's own zero value for
	// "nothing to migrate". A save never repopulates this field: once a file
	// has been through Load, Legacy is nil and stays out of the JSON entirely
	// (omitempty), which is what makes the migration idempotent without any
	// extra bookkeeping.
	Legacy       *VM      `json:"vm,omitempty"`
	Disks        []Disk   `json:"disks,omitempty"`
	ManagedDirs  []string `json:"managed_dirs,omitempty"`
	ManagedFiles []string `json:"managed_files,omitempty"`
	// Quarantined is never persisted (json:"-"). It is populated by Load, from
	// records this read could not trust; see QuarantinedVM.
	Quarantined []QuarantinedVM `json:"-"`
}

type Store struct {
	ConfigDir string
	CacheDir  string
	StatePath string
}

func DefaultStore() (*Store, error) {
	cfgRoot := os.Getenv("KAIROS_LAB_CONFIG_DIR")
	if cfgRoot == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("get user config dir: %w", err)
		}
		cfgRoot = d
	}
	cacheRoot := os.Getenv("KAIROS_LAB_CACHE_DIR")
	if cacheRoot == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("get user cache dir: %w", err)
		}
		cacheRoot = d
	}
	cfgDir := filepath.Join(cfgRoot, "kairos-lab")
	cacheDir := filepath.Join(cacheRoot, "kairos-lab")
	return &Store{
		ConfigDir: cfgDir,
		CacheDir:  cacheDir,
		StatePath: filepath.Join(cfgDir, "state.json"),
	}, nil
}

func NewState(s *Store) *State {
	st := &State{Version: SchemaVersion}
	st.ManagedDirs = uniqueSorted([]string{s.ConfigDir, s.CacheDir})
	return st
}

// Load reads state.json, migrating a pre-multi-VM (schema 1) file into the
// current shape and quarantining any VM record it cannot trust rather than
// failing the whole read. It is idempotent and safe to call with no Save to
// follow -- runStatus does exactly that -- because the migration only ever
// folds Legacy (present only in a genuine v1 file) into VMs, and Legacy is
// never written back once a file carries the current schema.
func (s *Store) Load() (*State, error) {
	b, err := os.ReadFile(s.StatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewState(s), nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	// A binary older than the file it is reading cannot know what a newer
	// schema version dropped or changed meaning under it, so it must refuse
	// rather than quietly write the file back out having silently discarded
	// whatever it did not understand.
	if st.Version > SchemaVersion {
		return nil, fmt.Errorf("state file at %s is schema version %d, newer than this binary supports (%d); upgrade kairos-lab before using this config directory", s.StatePath, st.Version, SchemaVersion)
	}

	migrateLegacyVM(&st)
	st.VMs, st.Quarantined = quarantineInvalidVMs(st.VMs)

	// Re-stamped unconditionally, not only when it was 0: every successful
	// Load has now brought the in-memory state fully up to the current
	// schema, whatever version the file on disk carried.
	st.Version = SchemaVersion
	st.ManagedDirs = uniqueSorted(append(st.ManagedDirs, s.ConfigDir, s.CacheDir))
	st.ManagedFiles = uniqueSorted(st.ManagedFiles)
	return &st, nil
}

// migrateLegacyVM folds a schema-1 "vm" record into the VMs list, keyed on
// Name so a file that somehow already carries a VMs entry of the same name
// is never duplicated -- the existing entry wins, since it is the newer
// schema's own data. Name falls back to DiskName and then to the base name
// of DiskPath, for a file recorded before DiskName existed; a record that
// still has neither is folded in with an empty Name; see runStart (M4) for
// how the "any VM" refusal that keeps such a record safe. Index is always 0:
// D7 makes index 0 byte-identical to a pre-multi-VM start, which is exactly
// what a migrated record describes.
func migrateLegacyVM(st *State) {
	if st.Legacy == nil {
		return
	}
	legacy := *st.Legacy
	st.Legacy = nil

	name := legacy.Name
	if name == "" {
		name = legacy.DiskName
	}
	if name == "" && legacy.DiskPath != "" {
		name = filepath.Base(legacy.DiskPath)
	}
	legacy.Name = name
	legacy.Index = 0

	for i := range st.VMs {
		if st.VMs[i].Name == name {
			return
		}
	}
	st.VMs = append(st.VMs, legacy)
}

// quarantineInvalidVMs partitions vms into the records Load can trust and the
// ones it cannot, per D9: a state.json is a 0644 file anything running as the
// user can write, so every field a VM record carries is validated on the way
// in, and a record that fails is set aside rather than allowed to fail the
// whole file or to reach a caller unchecked. Only Name, Index and
// NetworkMode are checked here; TapName, TapConnName and the two ports are
// deliberately not trusted at all -- internal/vm derives them fresh from
// Index at every point that matters (M2/M3/M4), so a corrupted copy of them
// in state.json can misinform status but cannot steer a root-run nmcli or ip
// command anywhere the index-derived name would not already have sent it.
func quarantineInvalidVMs(vms []VM) (valid []VM, quarantined []QuarantinedVM) {
	valid = make([]VM, 0, len(vms))
	for _, v := range vms {
		if reason := invalidVMReason(v); reason != "" {
			quarantined = append(quarantined, QuarantinedVM{Name: v.Name, Reason: reason})
			continue
		}
		valid = append(valid, v)
	}
	return valid, quarantined
}

// invalidVMReason returns why v cannot be trusted, or "" when it can.
//
// An empty Name is deliberately not one of the reasons: it is what a v1 file
// with no disk ever started migrates to (see migrateLegacyVM), and it is not
// adversarial -- nothing about it can steer a destructive command anywhere,
// since every such command is built from Index, not from Name. D9's
// whitelist is enforced everywhere a NEW name is chosen -- flag parsing, the
// interactive prompts -- which is where "no name" is actually a mistake
// rather than a compatibility fact.
func invalidVMReason(v VM) string {
	if v.Name != "" {
		if err := validDiskNameCharset(v.Name); err != nil {
			return fmt.Sprintf("name: %v", err)
		}
	}
	if v.Index < 0 || v.Index > MaxVMIndex {
		return fmt.Sprintf("index %d is outside the supported range 0..%d", v.Index, MaxVMIndex)
	}
	if v.NetworkMode != "" && !validNetworkMode(v.NetworkMode) {
		return fmt.Sprintf("network mode %q is not one this binary knows", v.NetworkMode)
	}
	return ""
}

// validNetworkMode mirrors internal/app's networkModes ("shared", "bridged",
// "user"). It is duplicated rather than imported because internal/app
// already imports internal/state, and importing back would cycle; the three
// values are part of the user-facing CLI surface and change exactly as often
// as that flag's help text does.
func validNetworkMode(mode string) bool {
	switch mode {
	case "shared", "bridged", "user":
		return true
	default:
		return false
	}
}

// validDiskNameCharset is D9's one whitelist -- [A-Za-z0-9._-], rejecting
// empty, ".", ".." and a leading '-' -- applied here on load, and by
// internal/app at flag-parse time and at the interactive prompts, so the
// write and read sides can never disagree about what name and Save wrote is
// a name Load's next read will accept. An empty string is rejected here
// because this function is only ever called with a non-empty v.Name (see
// invalidVMReason); a genuinely nameless record is a different, permitted
// case handled there, not here.
func validDiskNameCharset(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name is empty")
	case name == "." || name == "..":
		return fmt.Errorf("name %q is not allowed", name)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("name %q may not begin with '-'", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			continue
		}
		return fmt.Errorf("name %q may contain only letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

// Save publishes st at s.StatePath, by writing a complete temporary file
// beside it and renaming that over the name, so a concurrent reader sees
// either the whole old file or the whole new one and never something in
// between.
//
// No failure in here can shorten s.StatePath, and that is structural rather
// than tested: below, s.StatePath reaches the filesystem at exactly two
// places -- the Lstat that reads its mode and the Rename that replaces it --
// and neither can truncate. Reintroducing a truncation would mean
// reintroducing the in-place write this function exists to avoid.
// TestSaveFailureLeavesPreviousStateIntact pins the half of that a user can
// observe; the general property has no portable test, because the remaining
// ways the rename can fail either fail earlier at create time or run against
// a path with no previous file to compare.
func (s *Store) Save(st *State) error {
	if err := os.MkdirAll(s.ConfigDir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	st.ManagedDirs = uniqueSorted(append(st.ManagedDirs, s.ConfigDir, s.CacheDir))
	st.ManagedFiles = uniqueSorted(st.ManagedFiles)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize state: %w", err)
	}
	// The state file is published by renaming a complete temporary file over
	// it, not by writing into it in place. Writing in place truncates first, so
	// another process reading state.json at that moment -- a `kairos-lab
	// status` in a second terminal, say, while a running VM's IP address is
	// being recorded -- sees an empty or half-written file and fails to parse
	// it. A rename swaps the name onto already-complete contents in one step,
	// so every reader sees either the whole old file or the whole new one and
	// never something in between. That is also why the temporary file is
	// created in the state file's own directory rather than the system temp
	// dir: rename only works within a single filesystem -- across two it fails
	// outright with EXDEV rather than degrading to a copy -- and /tmp is
	// routinely a different one. The directory that has to match is the one
	// holding StatePath, not ConfigDir: Store is exported with exported fields
	// and nothing makes the two agree, so the only safe answer is the one the
	// rename will actually land in.
	tmp, err := createTempStateFile(filepath.Dir(s.StatePath))
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpPath := tmp.Name()
	closed := false
	renamed := false
	// Any error return below must leave no trace: the previous state.json is
	// still the live one, and a half-written temporary file next to it would be
	// nothing but litter in the user's config dir. It is the error returns that
	// are covered, and only those: anything that ends the process without
	// unwinding this frame -- a SIGKILL, an os.Exit, a panic on some other
	// goroutine -- between the create and the rename leaves a state.json.tmp-*
	// behind, because the file is already on disk and this defer never runs.
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	// Flush before the rename, so the name can never be published pointing at
	// contents the kernel has not yet put on disk.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync state file: %w", err)
	}
	// The mode the rename is about to publish has to come out the same way the
	// in-place write this function replaced left it, and that write had two
	// separate behaviours rather than one.
	//
	// A state.json that already existed kept whatever mode it had: an open with
	// O_CREATE|O_TRUNC and no O_EXCL ignores its mode argument for a file that
	// exists, so a user who ran `chmod 600` on their state.json stayed at 0600
	// across every later save. A rename publishes the temporary file's own mode
	// instead, which would silently undo that, so the carry below does it by
	// hand.
	//
	// A state.json that did not exist yet was created from the mode argument,
	// 0644, with the umask subtracted by the kernel -- which is what
	// createTempStateFile reproduces, and why it exists instead of a call to
	// os.CreateTemp. That branch is not a judgement about how private this file
	// ought to be; it is the behaviour of the code this rename replaced, kept
	// intact. It also has a user behind it: running the whole tool under sudo is
	// blessed on macOS, where the stock sudoers keeps HOME, so state.json can be
	// created by root inside the invoking user's config dir. At 0644 the user's
	// next unprivileged `status`, `reset` or `cleanup` can still read it; at
	// 0600 every one of them fails on EACCES -- Load falls back only for a file
	// that is absent, not for one it may not open -- and the way out is to
	// `sudo rm` the file by hand.
	//
	// Lstat rather than Stat, because a symlink at StatePath is replaced by the
	// rename rather than written through, so the mode of whatever it points at
	// is not the mode of anything published here. IsRegular for the same reason
	// from the other side: a StatePath that is a directory -- a rename onto it
	// can only fail -- would otherwise have its 0755 chmodded onto the temporary
	// file on the way to that failure.
	//
	// A stat that fails is not an error. The ordinary reason for it is that
	// there is no state.json yet, and that is exactly the case whose mode the
	// create already settled.
	if fi, err := os.Lstat(s.StatePath); err == nil && fi.Mode().IsRegular() {
		if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
			return fmt.Errorf("set state file mode: %w", err)
		}
	}
	// closed is set before the error is examined, not after: a Close that
	// reports an error has still given the descriptor back, so leaving the flag
	// false would have the deferred cleanup close the same file a second time.
	cerr := tmp.Close()
	closed = true
	if cerr != nil {
		return fmt.Errorf("close state file: %w", cerr)
	}
	if err := os.Rename(tmpPath, s.StatePath); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	renamed = true
	return nil
}

// tempStateFileAttempts bounds the search for an unused temporary name. Two
// saves would have to draw the same 64 random bits for even one retry to
// happen, so the bound is not about collisions: it is so that a directory
// which answers "that name exists" forever -- a filesystem bug, or a name that
// cannot be created for a reason the error does not distinguish -- ends in an
// error instead of a spin.
const tempStateFileAttempts = 10

// createTempStateFile opens a new, empty file in dir under a name of the form
// state.json.tmp-<hex>. It is os.CreateTemp with one difference, and that
// difference is the mode.
//
// os.CreateTemp hardcodes 0600 and offers no way to ask for anything else, so
// building on it capped every state.json created from scratch at 0600 instead
// of the 0644 the os.WriteFile this save path replaced asked for. What went
// missing was the ceiling and not the umask: os.CreateTemp hands its 0600 to
// the kernel like any other create mode, and a umask of 0277 duly turns it
// into 0400. Passing 0644 here restores the ceiling and leaves the subtraction
// where it already was. Doing that subtraction by hand instead is not an
// alternative: syscall.Umask is process-global, so zeroing it to read it
// corrupts the mode of any file another goroutine creates in that window, and
// it does not exist on Windows at all.
//
// O_EXCL is what makes the name safe rather than merely unused. It fails the
// open outright when anything already sits at the name, so an attacker who can
// write this directory and plants a symlink there cannot have this function
// follow it and put the state file wherever the link points -- and it is what
// gives the retry below something to retry. Drawing the suffix from
// crypto/rand rather than a counter or math/rand is the other half of that:
// a name nobody can predict is a name nobody can plant at.
func createTempStateFile(dir string) (*os.File, error) {
	for i := 0; i < tempStateFileAttempts; i++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, fmt.Errorf("generate a temporary name: %w", err)
		}
		name := filepath.Join(dir, "state.json.tmp-"+hex.EncodeToString(suffix[:]))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no unused name in %s after %d attempts", dir, tempStateFileAttempts)
}

func (s *Store) RemoveStateFile() error {
	err := os.Remove(s.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove state file: %w", err)
	}
	return nil
}

func AddManagedFile(st *State, path string) {
	st.ManagedFiles = uniqueSorted(append(st.ManagedFiles, path))
}

func AddManagedDir(st *State, path string) {
	st.ManagedDirs = uniqueSorted(append(st.ManagedDirs, path))
}

func RemoveManagedFile(st *State, path string) {
	out := make([]string, 0, len(st.ManagedFiles))
	for _, p := range st.ManagedFiles {
		if p != path {
			out = append(out, p)
		}
	}
	st.ManagedFiles = out
}

func NowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func NowTimestamp() string {
	return time.Now().Format("20060102-150405")
}

func AddDisk(st *State, disk Disk) {
	st.Disks = append(st.Disks, disk)
}

func FindDiskByName(st *State, name string) *Disk {
	for i := range st.Disks {
		if st.Disks[i].Name == name {
			return &st.Disks[i]
		}
	}
	return nil
}

func RemoveDisk(st *State, name string) {
	out := make([]Disk, 0, len(st.Disks))
	for _, d := range st.Disks {
		if d.Name != name {
			out = append(out, d)
		}
	}
	st.Disks = out
}

// FindVM returns the VM record named name, or nil when there is none. Name is
// D1's identity for a VM.
func FindVM(st *State, name string) *VM {
	for i := range st.VMs {
		if st.VMs[i].Name == name {
			return &st.VMs[i]
		}
	}
	return nil
}

// UpsertVM replaces the VM record named v.Name, or appends v when there is no
// existing record of that name.
func UpsertVM(st *State, v VM) {
	for i := range st.VMs {
		if st.VMs[i].Name == v.Name {
			st.VMs[i] = v
			return
		}
	}
	st.VMs = append(st.VMs, v)
}

// RemoveVM removes the VM record named name, if there is one.
func RemoveVM(st *State, name string) {
	out := make([]VM, 0, len(st.VMs))
	for _, v := range st.VMs {
		if v.Name != name {
			out = append(out, v)
		}
	}
	st.VMs = out
}

// NextFreeVMIndex returns the lowest index in 0..MaxVMIndex not held by a
// live VM, per D6. "Not held by a live VM" and not "not held by any VM": a
// config dir with two disks started one at a time would otherwise give the
// second disk a rising index and non-default ports with only one VM ever
// running, which is the ordinary case reset -disk and -new exist for. live
// reports whether a given record currently counts as live; this package has
// no process to signal and no host to probe, so it takes that answer from
// the caller rather than deciding it -- see D3 and D4 for what "live" means.
func NextFreeVMIndex(st *State, live func(VM) bool) (int, error) {
	held := make(map[int]bool, len(st.VMs))
	count := 0
	for _, v := range st.VMs {
		if live(v) {
			held[v.Index] = true
			count++
		}
	}
	for i := 0; i <= MaxVMIndex; i++ {
		if !held[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free VM index in 0..%d: %d VMs are live", MaxVMIndex, count)
}

// lockAcquireTimeout and lockRetryInterval bound Update's wait for the
// exclusive flock, per D8: a blocking acquire would let one wedged process
// make reset and cleanup -- the recovery commands -- unusable, and a
// re-entrant call (Update invoked again, on the same Store, from inside its
// own fn, in the goroutine that is still holding the lock) would otherwise
// hang for the full timeout rather than failing fast. Both are vars, not
// constants, so a test can shrink them and observe that second behaviour in
// well under a second instead of thirty.
var (
	lockAcquireTimeout = 30 * time.Second
	lockRetryInterval  = 50 * time.Millisecond
)

// Update runs fn against freshly loaded state and saves the result back, the
// load and the save both happening under one exclusive flock on the state
// directory so two processes -- or two goroutines -- racing a start never
// interleave their reads and writes. See D8 for the reasoning behind the
// lock file's open mode, why O_NOFOLLOW, and the limits this does not
// solve (an NFS mount, a cleanup that unlinks the lock file with
// os.RemoveAll).
//
// fn's error is returned unsaved: a validation failure inside fn must not
// publish a half-updated state.json. A panic inside fn is not recovered here
// on purpose -- runStart has no revert of its own after command.Start(), so
// swallowing a panic into a returned error here would hide exactly the
// caller that most needs to see one.
func (s *Store) Update(fn func(*State) error) error {
	lockPath := filepath.Join(filepath.Dir(s.StatePath), ".state.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	unlock, err := acquireLock(lockPath, lockAcquireTimeout, lockRetryInterval)
	if err != nil {
		return fmt.Errorf("acquire state lock: %w", err)
	}
	defer func() { _ = unlock() }()

	st, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.Save(st)
}

func IsSetupComplete(st *State) bool {
	return st.Setup.CompletedAt != "" && st.Setup.DependencyCheckPassed
}

func uniqueSorted(values []string) []string {
	set := map[string]struct{}{}
	for _, v := range values {
		if v == "" {
			continue
		}
		set[v] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
