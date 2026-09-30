package auroraboot

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const stubRuntimeScript = `#!/bin/sh
case "$1" in
info)
	if [ "$2" = "--format" ]; then
		printf '%s\n' "$STUB_INFO_SOCKET"
	fi
	exit "${STUB_INFO_EXIT:-0}"
	;;
--version)
	printf '%s\n' "$STUB_VERSION"
	exit 0
	;;
context)
	printf '%s\n' "$STUB_CONTEXT_HOST"
	exit 0
	;;
run)
	for a in "$@"; do
		printf '%s\0' "$a" >>"$STUB_RECORD"
	done
	exit "${STUB_EXIT:-0}"
	;;
esac
exit 99
`

type shimResult struct {
	stdout string
	stderr string
	exit   int
	// argv is what the runtime received, starting with "run". It is nil when
	// the runtime was never called.
	argv []string
}

// shimOpts carries the environment a shim run sees. Zero values are fine.
type shimOpts struct {
	stubExit    int
	dockerHost  string
	contextHost string
	infoSocket  string
}

// tempDir returns a symlink-free temporary directory: the shim resolves paths
// physically, and on macOS t.TempDir() sits behind /var to /private/var.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// shortSocketDir returns a directory short enough for a unix socket path
// (macOS allows 104 bytes).
func shortSocketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "kl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, err = filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func listenUnix(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

func renderTo(t *testing.T, goos, runtime string) string {
	t.Helper()
	b, err := RenderShim(runtime, ImageRef(), goos)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(tempDir(t), "auroraboot")
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// runShim renders the shim for goos and runtime, puts a stub runtime first on
// PATH, and runs the script in cwd with args. The stub exits with stubExit
// from `run` and records the argv it got.
func runShim(t *testing.T, goos, runtime string, opts shimOpts, cwd string, args ...string) shimResult {
	t.Helper()
	script := renderTo(t, goos, runtime)
	stubDir := tempDir(t)
	if err := os.WriteFile(filepath.Join(stubDir, runtime), []byte(stubRuntimeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(tempDir(t), "record")
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"PATH=" + stubDir + ":/usr/bin:/bin",
		"HOME=" + tempDir(t),
		"STUB_RECORD=" + record,
		"STUB_EXIT=" + strconv.Itoa(opts.stubExit),
		"STUB_CONTEXT_HOST=" + opts.contextHost,
		"STUB_INFO_SOCKET=" + opts.infoSocket,
	}
	if opts.dockerHost != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+opts.dockerHost)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := shimResult{stdout: stdout.String(), stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.exit = ee.ExitCode()
	default:
		t.Fatalf("running the shim: %v", err)
	}
	if b, rerr := os.ReadFile(record); rerr == nil {
		res.argv = strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	}
	return res
}

// hasSeq reports whether want appears in argv as consecutive elements.
func hasSeq(argv []string, want ...string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		match := true
		for j, w := range want {
			if argv[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func has(argv []string, want string) bool { return hasSeq(argv, want) }

func TestShimRunsPinnedImageInCWD(t *testing.T) {
	cwd := tempDir(t)
	res := runShim(t, "linux", "docker", shimOpts{}, cwd, "build-iso", "--output", "./build", "quay.io/kairos/alpine:3.21")
	if res.exit != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.exit, res.stderr)
	}
	if res.argv[0] != "run" {
		t.Fatalf("first runtime arg = %q, want run", res.argv[0])
	}
	for _, want := range [][]string{{"--rm"}, {"-i"}, {"-w", cwd}, {"-v", cwd + ":" + cwd}} {
		if !hasSeq(res.argv, want...) {
			t.Errorf("argv %q is missing %q", res.argv, want)
		}
	}
	// The image is the pinned one, followed by the arguments word for word.
	tail := []string{ImageRef(), "build-iso", "--output", "./build", "quay.io/kairos/alpine:3.21"}
	got := res.argv[len(res.argv)-len(tail):]
	if strings.Join(got, "\x00") != strings.Join(tail, "\x00") {
		t.Errorf("argv tail = %q, want %q", got, tail)
	}
	for _, a := range res.argv {
		if a == "--privileged" || strings.Contains(a, ":latest") {
			t.Errorf("unexpected argument %q", a)
		}
	}
}

func TestShimNoTTYOmitsT(t *testing.T) {
	res := runShim(t, "linux", "docker", shimOpts{}, tempDir(t), "genkey")
	if has(res.argv, "-t") || has(res.argv, "-it") {
		t.Errorf("argv %q has -t although stdin and stdout are not a terminal", res.argv)
	}
	if !has(res.argv, "-i") {
		t.Errorf("argv %q lacks -i", res.argv)
	}
}

func TestShimPassesExitCode(t *testing.T) {
	res := runShim(t, "linux", "docker", shimOpts{stubExit: 42}, tempDir(t), "genkey")
	if res.exit != 42 {
		t.Fatalf("exit = %d, want 42 from the runtime", res.exit)
	}
}

func TestShimLinuxOnlyOptions(t *testing.T) {
	linux := runShim(t, "linux", "docker", shimOpts{}, tempDir(t), "genkey")
	if !hasSeq(linux.argv, "--net", "host") || !hasSeq(linux.argv, "--security-opt", "label=disable") {
		t.Errorf("linux argv %q lacks --net host or --security-opt label=disable", linux.argv)
	}
	darwin := runShim(t, "darwin", "docker", shimOpts{}, tempDir(t), "genkey")
	if has(darwin.argv, "--net") || has(darwin.argv, "--security-opt") {
		t.Errorf("darwin argv %q carries linux-only options", darwin.argv)
	}
	for _, res := range []shimResult{linux, darwin} {
		if has(res.argv, "--privileged") || has(res.argv, "--init") {
			t.Errorf("argv %q carries --privileged or --init", res.argv)
		}
	}
}

func TestShimMountsSocketOnlyWhenPresent(t *testing.T) {
	sockDir := shortSocketDir(t)
	sock := filepath.Join(sockDir, "d.sock")
	listenUnix(t, sock)
	missing := filepath.Join(sockDir, "gone.sock")
	mount := func(p string) []string { return []string{"-v", p + ":/var/run/docker.sock"} }

	// DOCKER_HOST wins and its socket exists.
	res := runShim(t, "linux", "docker", shimOpts{dockerHost: "unix://" + sock}, tempDir(t), "genkey")
	if !hasSeq(res.argv, mount(sock)...) {
		t.Errorf("DOCKER_HOST socket not mounted: %q", res.argv)
	}
	// DOCKER_HOST names a socket that is not there.
	res = runShim(t, "linux", "docker", shimOpts{dockerHost: "unix://" + missing}, tempDir(t), "genkey")
	if has(res.argv, "/var/run/docker.sock") || strings.Contains(strings.Join(res.argv, " "), "docker.sock") {
		t.Errorf("a missing socket was mounted: %q", res.argv)
	}
	// No DOCKER_HOST: the active context says where the socket is.
	res = runShim(t, "linux", "docker", shimOpts{contextHost: "unix://" + sock}, tempDir(t), "genkey")
	if !hasSeq(res.argv, mount(sock)...) {
		t.Errorf("context socket not mounted: %q", res.argv)
	}
	// A context on a remote host has no local socket.
	res = runShim(t, "linux", "docker", shimOpts{contextHost: "ssh://box"}, tempDir(t), "genkey")
	if strings.Contains(strings.Join(res.argv, " "), "docker.sock") {
		t.Errorf("a non-unix context produced a socket mount: %q", res.argv)
	}
	// podman on Linux asks podman where its socket is.
	res = runShim(t, "linux", "podman", shimOpts{infoSocket: sock}, tempDir(t), "genkey")
	if !hasSeq(res.argv, mount(sock)...) {
		t.Errorf("podman socket not mounted: %q", res.argv)
	}
	// Docker Desktop on macOS: the socket lives in the VM, so it is mounted
	// without a host check.
	res = runShim(t, "darwin", "docker", shimOpts{}, tempDir(t), "genkey")
	if !hasSeq(res.argv, mount("/var/run/docker.sock")...) {
		t.Errorf("darwin docker socket not mounted: %q", res.argv)
	}
	// podman on macOS never gets one.
	res = runShim(t, "darwin", "podman", shimOpts{infoSocket: sock}, tempDir(t), "genkey")
	if strings.Contains(strings.Join(res.argv, " "), "docker.sock") {
		t.Errorf("darwin podman mounted a socket: %q", res.argv)
	}
}

func TestShimBuildISOWithoutOutputWarnsAndPassesThrough(t *testing.T) {
	cwd := tempDir(t)
	args := []string{"build-iso", "quay.io/kairos/alpine:3.21"}
	res := runShim(t, "linux", "docker", shimOpts{}, cwd, args...)
	if res.exit != 0 || res.argv == nil {
		t.Fatalf("the runtime must still be called; exit=%d argv=%q stderr=%q", res.exit, res.argv, res.stderr)
	}
	if n := strings.Count(res.stderr, "\n"); n != 1 || !strings.Contains(res.stderr, "--output") {
		t.Errorf("want exactly one warning line naming --output, got %q", res.stderr)
	}
	tail := append([]string{ImageRef()}, args...)
	got := res.argv[len(res.argv)-len(tail):]
	if strings.Join(got, "\x00") != strings.Join(tail, "\x00") {
		t.Errorf("arguments changed: got %q, want %q", got, tail)
	}
	for _, quiet := range [][]string{
		{"build-iso", "--output", "out", "img"},
		{"bi", "-o", "out", "img"},
		{"build-iso", "--output=out", "img"},
		{"genkey"},
		{"--set", "build-iso=1", "genkey"},
	} {
		if res := runShim(t, "linux", "docker", shimOpts{}, cwd, quiet...); res.stderr != "" {
			t.Errorf("%q printed %q, want no warning", quiet, res.stderr)
		}
	}
	if res := runShim(t, "linux", "docker", shimOpts{}, cwd, "bi", "img"); res.stderr == "" {
		t.Error("the bi alias did not warn")
	}
}

func TestShimRefusesReservedCWD(t *testing.T) {
	for _, cwd := range []string{"/", "/etc", "/usr"} {
		if _, err := os.Stat(cwd); err != nil {
			continue
		}
		res := runShim(t, "linux", "docker", shimOpts{}, cwd, "genkey")
		if res.exit != 2 {
			t.Errorf("cwd %s: exit = %d, want 2", cwd, res.exit)
		}
		if res.argv != nil {
			t.Errorf("cwd %s: the runtime was called: %q", cwd, res.argv)
		}
		if !strings.Contains(res.stderr, cwd) {
			t.Errorf("cwd %s: stderr %q does not name it", cwd, res.stderr)
		}
	}
	colon := filepath.Join(tempDir(t), "a:b")
	if err := os.Mkdir(colon, 0o755); err != nil {
		t.Fatal(err)
	}
	if res := runShim(t, "linux", "docker", shimOpts{}, colon, "genkey"); res.exit != 2 || res.argv != nil {
		t.Errorf("a cwd containing a colon: exit=%d argv=%q, want exit 2 and no run", res.exit, res.argv)
	}
}

func TestRenderShimRejectsBadInputs(t *testing.T) {
	good, err := RenderShim("docker", ImageRef(), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(good, []byte("\n"+ShimMarker+"\n")) {
		t.Error("the rendered shim lacks its marker line")
	}
	if bytes.Contains(good, []byte("@@")) {
		t.Error("a placeholder was left in the rendered shim")
	}
	for _, tc := range []struct{ runtime, image, goos string }{
		{"/tmp/evil", ImageRef(), "linux"},
		{"docker; rm -rf ~", ImageRef(), "linux"},
		{"", ImageRef(), "linux"},
		{"docker", "quay.io/kairos/other:v1", "linux"},
		{"docker", ImageRef() + "\n", "linux"},
		{"docker", "", "linux"},
		{"docker", ImageRef(), "windows"},
		{"docker", ImageRef(), ""},
	} {
		if _, err := RenderShim(tc.runtime, tc.image, tc.goos); err == nil {
			t.Errorf("RenderShim(%q, %q, %q) succeeded, want an error", tc.runtime, tc.image, tc.goos)
		}
	}
}
