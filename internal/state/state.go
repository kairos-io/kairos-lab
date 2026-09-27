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
	"time"
)

const SchemaVersion = 1

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

type VM struct {
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
}

type State struct {
	Version      int      `json:"version"`
	Platform     Platform `json:"platform"`
	Setup        Setup    `json:"setup"`
	Network      Network  `json:"network"`
	VM           VM       `json:"vm"`
	Disks        []Disk   `json:"disks,omitempty"`
	ManagedDirs  []string `json:"managed_dirs,omitempty"`
	ManagedFiles []string `json:"managed_files,omitempty"`
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
	if st.Version == 0 {
		st.Version = SchemaVersion
	}
	st.ManagedDirs = uniqueSorted(append(st.ManagedDirs, s.ConfigDir, s.CacheDir))
	st.ManagedFiles = uniqueSorted(st.ManagedFiles)
	return &st, nil
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
