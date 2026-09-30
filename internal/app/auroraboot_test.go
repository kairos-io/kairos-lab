package app

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-lab/internal/auroraboot"
	"github.com/kairos-io/kairos-lab/internal/platform"
	"github.com/kairos-io/kairos-lab/internal/state"
)

// runtimeStub is a stand-in for a container runtime binary. It logs every call
// to $STUB_LOG, answers `info` and `--version`, treats the file "image" in
// $STUB_DIR as the presence of the image, creates it on a successful pull, and
// notes in "state-had-ref" whether state.json already named the image when the
// pull started.
const runtimeStub = `#!/bin/sh
echo "$NAME $*" >> "$STUB_LOG"
case "$1" in
info) exit "${INFO_EXIT:-0}" ;;
--version) echo "$VERSION_OUT"; exit 0 ;;
image)
	case "$2" in
	inspect) [ -f "$STUB_DIR/image" ] && exit 0; exit 1 ;;
	rm) rm -f "$STUB_DIR/image" 2>/dev/null; exit "${RM_EXIT:-0}" ;;
	esac
	;;
pull)
	while read -r line; do
		case $line in
		*auroraboot:v*) : > "$STUB_DIR/state-had-ref" ;;
		esac
	done < "$STATE_FILE"
	[ "${PULL_EXIT:-0}" = 0 ] && : > "$STUB_DIR/image"
	exit "${PULL_EXIT:-0}"
	;;
esac
exit 0
`

type abEnv struct {
	home    string
	bin     string
	stubDir string
	log     string
	store   *state.Store
}

// newABEnv isolates HOME, the state directories and PATH, and provides the
// binaries the existing dependency check looks for, so setup gets past it
// without touching the host.
func newABEnv(t *testing.T) *abEnv {
	t.Helper()
	e := &abEnv{home: t.TempDir(), bin: t.TempDir(), stubDir: t.TempDir()}
	t.Setenv("HOME", e.home)
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	t.Setenv("PATH", e.bin)
	e.log = filepath.Join(e.stubDir, "log")
	t.Setenv("STUB_LOG", e.log)
	t.Setenv("STUB_DIR", e.stubDir)
	store, err := state.DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	e.store = store
	t.Setenv("STATE_FILE", store.StatePath)
	for _, b := range []string{"qemu-img", "qemu-system-x86_64", "qemu-system-aarch64", "ip"} {
		e.write(t, b, "#!/bin/sh\nexit 0\n")
	}
	oldPlatform, oldInstall := detectPlatform, installPackages
	t.Cleanup(func() { detectPlatform, installPackages = oldPlatform, oldInstall })
	installPackages = func(pm string, pkgs []string, useSudo bool) error {
		t.Errorf("installPackages(%s, %v) called unexpectedly", pm, pkgs)
		return nil
	}
	return e
}

func (e *abEnv) write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(e.bin, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// runtime installs a runtime stub named name on PATH.
func (e *abEnv) runtime(t *testing.T, name string) {
	t.Helper()
	e.write(t, name, strings.Replace(runtimeStub, "$NAME", name, 1))
}

func (e *abEnv) hasImage(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.stubDir, "image"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *abEnv) logText() string {
	b, _ := os.ReadFile(e.log)
	return string(b)
}

func (e *abEnv) load(t *testing.T) *state.State {
	t.Helper()
	st, err := e.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *abEnv) setup(stdin string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	err := Run(append([]string{"setup"}, args...), strings.NewReader(stdin), &stdout, &stderr, "test")
	return stdout.String(), err
}

func TestSetupRecordsPulledImageAndShim(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	ref := auroraboot.ImageRef()
	st := e.load(t)
	ab := st.AuroraBoot
	shimDir := filepath.Join(e.home, ".local", "bin")
	if ab.Runtime != "docker" {
		t.Errorf("runtime = %q, want docker", ab.Runtime)
	}
	if !slices.Equal(ab.PulledImages, []string{ref}) {
		t.Errorf("pulled images = %v, want [%s]", ab.PulledImages, ref)
	}
	if len(ab.PreExistingImages) != 0 {
		t.Errorf("pre-existing images = %v, want none", ab.PreExistingImages)
	}
	if want := filepath.Join(shimDir, "auroraboot"); ab.ShimPath != want {
		t.Errorf("shim path = %q, want %q", ab.ShimPath, want)
	}
	if ab.ShimDirCreated != shimDir {
		t.Errorf("shim dir created = %q, want %q", ab.ShimDirCreated, shimDir)
	}
	if !auroraboot.IsManagedShim(ab.ShimPath) {
		t.Error("the installed shim lacks its marker")
	}
	if !slices.Contains(st.Setup.PreExistingDeps, "docker") {
		t.Errorf("pre-existing deps = %v, want docker in them", st.Setup.PreExistingDeps)
	}
	if !strings.Contains(e.logText(), "docker pull "+ref) {
		t.Errorf("no pull of %s in %q", ref, e.logText())
	}
	if _, err := os.Stat(filepath.Join(e.stubDir, "state-had-ref")); err != nil {
		t.Error("state.json did not name the image when the pull started")
	}
	if !strings.Contains(out, "export PATH=") {
		t.Errorf("no PATH hint although %s is not on PATH:\n%s", shimDir, out)
	}
	if !strings.Contains(out, "[5/5]") {
		t.Errorf("no step 5 line:\n%s", out)
	}

	// A second run rewrites the shim and keeps every record.
	if out, err := e.setup("", "-yes"); err != nil {
		t.Fatalf("second setup: %v\n%s", err, out)
	}
	again := e.load(t).AuroraBoot
	if again.ShimDirCreated != shimDir || !slices.Equal(again.PulledImages, []string{ref}) || len(again.PreExistingImages) != 0 {
		t.Errorf("second run changed the record: %+v", again)
	}
}

func TestSetupKeepsPreExistingImageUntracked(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	e.hasImage(t)
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	ref := auroraboot.ImageRef()
	ab := e.load(t).AuroraBoot
	if len(ab.PulledImages) != 0 {
		t.Errorf("pulled images = %v, want none for an image that was already there", ab.PulledImages)
	}
	if !slices.Equal(ab.PreExistingImages, []string{ref}) {
		t.Errorf("pre-existing images = %v, want [%s]", ab.PreExistingImages, ref)
	}
	if strings.Contains(e.logText(), "docker pull") {
		t.Errorf("the image was pulled although present: %q", e.logText())
	}
	if ab.ShimPath == "" {
		t.Error("the shim was not installed")
	}
}

func TestSetupSkipsWhenAuroraBootOnPath(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	mine := "#!/bin/sh\necho mine\n"
	e.write(t, "auroraboot", mine)
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if ab := e.load(t).AuroraBoot; !isZeroAuroraBoot(ab) {
		t.Errorf("state recorded %+v, want nothing", ab)
	}
	if !strings.Contains(out, "already installed at "+filepath.Join(e.bin, "auroraboot")) {
		t.Errorf("output does not say why:\n%s", out)
	}
	if strings.Contains(e.logText(), "pull") {
		t.Errorf("the image was pulled for a shim that will not be written: %q", e.logText())
	}
	if b, _ := os.ReadFile(filepath.Join(e.bin, "auroraboot")); string(b) != mine {
		t.Error("the user's auroraboot was modified")
	}
}

func TestAuroraBootStepSkipsWhenShimPathHoldsAForeignFile(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	target := filepath.Join(e.home, ".local", "bin", "auroraboot")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if ab := e.load(t).AuroraBoot; !isZeroAuroraBoot(ab) {
		t.Errorf("state recorded %+v, want nothing", ab)
	}
	if b, _ := os.ReadFile(target); string(b) != "#!/bin/sh\necho mine\n" {
		t.Error("the file at the shim path was modified")
	}
}

func isZeroAuroraBoot(ab state.AuroraBoot) bool {
	return ab.Runtime == "" && ab.ShimPath == "" && ab.ShimDirCreated == "" &&
		len(ab.PulledImages) == 0 && len(ab.PreExistingImages) == 0
}

func TestSetupNoAuroraBootFlag(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	out, err := e.setup("", "-yes", "-no-auroraboot")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if e.logText() != "" {
		t.Errorf("the runtime was called: %q", e.logText())
	}
	if ab := e.load(t).AuroraBoot; !isZeroAuroraBoot(ab) {
		t.Errorf("state recorded %+v, want nothing", ab)
	}
	if _, err := os.Lstat(filepath.Join(e.home, ".local")); err == nil {
		t.Error("~/.local was created")
	}
	if !e.load(t).Setup.DependencyCheckPassed {
		t.Error("the rest of setup did not complete")
	}
}

func TestSetupSkipsWithoutRuntimeOnDarwin(t *testing.T) {
	e := newABEnv(t)
	detectPlatform = func() platform.Info {
		return platform.Info{OS: "darwin", Arch: "arm64", PackageManager: "brew"}
	}
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Docker Desktop") {
		t.Errorf("no install hint:\n%s", out)
	}
	if ab := e.load(t).AuroraBoot; !isZeroAuroraBoot(ab) {
		t.Errorf("state recorded %+v, want nothing", ab)
	}
	if _, err := os.Lstat(filepath.Join(e.home, ".local")); err == nil {
		t.Error("~/.local was created")
	}
}

func TestSetupSavesStateBeforePullFailure(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	t.Setenv("PULL_EXIT", "1")
	out, err := e.setup("", "-yes")
	if err == nil {
		t.Fatalf("a failed pull was not reported:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(e.stubDir, "state-had-ref")); serr != nil {
		t.Error("state.json did not name the image when the pull started")
	}
	st := e.load(t)
	if st.AuroraBoot.Runtime != "docker" || !slices.Contains(st.Setup.PreExistingDeps, "docker") {
		t.Errorf("the runtime was not saved: %+v %v", st.AuroraBoot, st.Setup.PreExistingDeps)
	}
	if len(st.AuroraBoot.PulledImages) != 0 {
		t.Errorf("pulled images = %v, want none after a failed pull", st.AuroraBoot.PulledImages)
	}
	if st.AuroraBoot.ShimPath != "" {
		t.Error("a shim was recorded although the pull failed")
	}
	if !st.Setup.DependencyCheckPassed {
		t.Error("the failed auroraboot step undid the rest of setup")
	}
}

func TestSetupRuntimeFlagChoosesPodman(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	e.runtime(t, "podman")
	out, err := e.setup("", "-yes", "-runtime", "podman")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if got := e.load(t).AuroraBoot.Runtime; got != "podman" {
		t.Errorf("runtime = %q, want podman", got)
	}
	log := e.logText()
	if !strings.Contains(log, "podman pull "+auroraboot.ImageRef()) || strings.Contains(log, "docker pull") {
		t.Errorf("wrong runtime pulled: %q", log)
	}
	b, err := os.ReadFile(e.load(t).AuroraBoot.ShimPath)
	if err != nil || !strings.Contains(string(b), "runtime='podman'") {
		t.Errorf("the shim does not run podman: %v", err)
	}

	// With no flag and both usable, docker is the default.
	e2 := newABEnv(t)
	e2.runtime(t, "docker")
	e2.runtime(t, "podman")
	if out, err := e2.setup("", "-yes"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if got := e2.load(t).AuroraBoot.Runtime; got != "docker" {
		t.Errorf("default runtime = %q, want docker", got)
	}

	if _, err := e2.setup("", "-yes", "-runtime", "nerdctl"); err == nil {
		t.Error("an unsupported -runtime was accepted")
	}
}

func TestAuroraBootStepDecliningThePullSkipsWithoutFailing(t *testing.T) {
	e := newABEnv(t)
	e.runtime(t, "docker")
	out, err := e.setup("n\n")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2.2 GB") {
		t.Errorf("the prompt does not state the size:\n%s", out)
	}
	if strings.Contains(e.logText(), "docker pull") {
		t.Error("the image was pulled after declining")
	}
	st := e.load(t)
	if st.AuroraBoot.ShimPath != "" || len(st.AuroraBoot.PulledImages) != 0 {
		t.Errorf("state recorded %+v after declining", st.AuroraBoot)
	}
	if !st.Setup.DependencyCheckPassed {
		t.Error("declining undid the rest of setup")
	}
}

func TestAuroraBootStepInstallsARuntimeOnlyWhenNoneExists(t *testing.T) {
	e := newABEnv(t)
	detectPlatform = func() platform.Info {
		return platform.Info{OS: "linux", Arch: "amd64", PackageManager: "apt"}
	}
	var installed [][]string
	installPackages = func(pm string, pkgs []string, useSudo bool) error {
		if pm != "apt" || !useSudo {
			t.Errorf("install ran as pm=%s sudo=%v", pm, useSudo)
		}
		installed = append(installed, pkgs)
		e.runtime(t, "docker")
		return nil
	}
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if len(installed) != 1 || !slices.Equal(installed[0], []string{"docker.io"}) {
		t.Errorf("installed %v, want [[docker.io]]", installed)
	}
	st := e.load(t)
	if !slices.Contains(st.Setup.InstalledByKairosLab, "docker") {
		t.Errorf("installed = %v, want docker tracked", st.Setup.InstalledByKairosLab)
	}
	if slices.Contains(st.Setup.PreExistingDeps, "docker") {
		t.Errorf("pre-existing = %v: a runtime setup installed must stay removable", st.Setup.PreExistingDeps)
	}
	if st.AuroraBoot.Runtime != "docker" || st.AuroraBoot.ShimPath == "" {
		t.Errorf("auroraboot state = %+v", st.AuroraBoot)
	}

	// A second run finds docker present and must not mark it pre-existing.
	installPackages = func(string, []string, bool) error {
		t.Error("installed again")
		return nil
	}
	if out, err := e.setup("", "-yes"); err != nil {
		t.Fatalf("second setup: %v\n%s", err, out)
	}
	if st := e.load(t); slices.Contains(st.Setup.PreExistingDeps, "docker") {
		t.Errorf("a re-run made the installed runtime pre-existing: %v", st.Setup.PreExistingDeps)
	}
}

func TestAuroraBootStepInstallsPodmanWhenAsked(t *testing.T) {
	e := newABEnv(t)
	detectPlatform = func() platform.Info {
		return platform.Info{OS: "linux", Arch: "amd64", PackageManager: "dnf"}
	}
	var got []string
	installPackages = func(pm string, pkgs []string, useSudo bool) error {
		got = pkgs
		e.runtime(t, "podman")
		return nil
	}
	if out, err := e.setup("", "-yes", "-runtime", "podman"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !slices.Equal(got, []string{"podman"}) {
		t.Errorf("installed %v, want [podman]", got)
	}
	if !slices.Contains(e.load(t).Setup.InstalledByKairosLab, "podman") {
		t.Error("podman was not tracked as installed")
	}
}

func TestAuroraBootStepNeverInstallsOverABrokenRuntime(t *testing.T) {
	e := newABEnv(t)
	detectPlatform = func() platform.Info {
		return platform.Info{OS: "linux", Arch: "amd64", PackageManager: "apt"}
	}
	e.runtime(t, "docker")
	t.Setenv("INFO_EXIT", "1")
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "docker info") {
		t.Errorf("output does not say why docker is unusable:\n%s", out)
	}
	if ab := e.load(t).AuroraBoot; !isZeroAuroraBoot(ab) {
		t.Errorf("state recorded %+v, want nothing", ab)
	}
}

func TestAuroraBootStepExplainsAnInstalledDockerThatDoesNotWorkYet(t *testing.T) {
	e := newABEnv(t)
	detectPlatform = func() platform.Info {
		return platform.Info{OS: "linux", Arch: "amd64", PackageManager: "pacman"}
	}
	installPackages = func(pm string, pkgs []string, useSudo bool) error {
		e.runtime(t, "docker")
		t.Setenv("INFO_EXIT", "1")
		return nil
	}
	out, err := e.setup("", "-yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"systemctl enable --now docker", "usermod -aG docker", "does not change groups"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	st := e.load(t)
	if !slices.Contains(st.Setup.InstalledByKairosLab, "docker") {
		t.Error("the installed docker was not tracked")
	}
	if st.AuroraBoot.ShimPath != "" {
		t.Error("a shim was written for a runtime that does not work")
	}
}
