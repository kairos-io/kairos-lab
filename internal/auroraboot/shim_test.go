package auroraboot

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
		// The message names the directory as resolved too, and on macOS
		// /etc resolves to /private/etc, so either spelling will do.
		names := []string{cwd}
		if phys, err := filepath.EvalSymlinks(cwd); err == nil {
			names = append(names, phys)
		}
		named := false
		for _, n := range names {
			named = named || strings.Contains(res.stderr, n)
		}
		if !named {
			t.Errorf("cwd %s: stderr %q names none of %q", cwd, res.stderr, names)
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

// mountedPaths returns the host paths the shim mounted at themselves, other
// than the working directory and the runtime socket.
func mountedPaths(argv []string, cwd string) []string {
	var out []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] != "-v" {
			continue
		}
		src, dst, ok := strings.Cut(argv[i+1], ":")
		if !ok || src != dst || src == cwd {
			continue
		}
		out = append(out, src)
	}
	sort.Strings(out)
	return out
}

func TestShimPathRules(t *testing.T) {
	mk := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, p := range paths {
			if strings.HasSuffix(p, "/") {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name string
		// setup runs with root = the parent of the working directory and
		// returns the arguments and the expected extra mounts.
		setup func(t *testing.T, root string) (args []string, mounts []string)
		exit  int
		// created lists paths under root that must exist afterwards
		// (created=true) or must not (created=false).
		created map[string]bool
	}{
		{
			name: "relative under cwd needs no mount",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/work/rootfs/")
				return []string{"build-iso", "--output", "./build", "--overlay-rootfs", "rootfs", "img"}, nil
			},
			created: map[string]bool{"work/build": false},
		},
		{
			name: "output outside cwd is created and mounted",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--output", "../out", "img"}, []string{root + "/out"}
			},
			created: map[string]bool{"out": true},
		},
		{
			name: "output with equals and an absolute path",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--output=" + root + "/abs/deep", "img"}, []string{root + "/abs/deep"}
			},
			created: map[string]bool{"abs/deep": true},
		},
		{
			name: "short output flag with equals",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"bi", "-o=../short", "img"}, []string{root + "/short"}
			},
			created: map[string]bool{"short": true},
		},
		{
			name: "build-uki -o is an input and is never created",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/rootfs/")
				return []string{"build-uki", "-o", "../rootfs", "-d", "../uki", "img"}, []string{root + "/rootfs", root + "/uki"}
			},
			created: map[string]bool{"uki": true},
		},
		{
			name: "missing input is left for auroraboot to report",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-uki", "--overlay-rootfs", "../nope", "img"}, nil
			},
			created: map[string]bool{"nope": false},
		},
		{
			name: "dir source",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/rootfs/")
				return []string{"build-iso", "--output", "o", "dir:../rootfs"}, []string{root + "/rootfs"}
			},
		},
		{
			name: "dir source with an absolute path after two slashes",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/rootfs/")
				return []string{"build-iso", "--output", "o", "dir://" + root + "/rootfs"}, []string{root + "/rootfs"}
			},
		},
		{
			name: "file source",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/files/a.tar")
				return []string{"build-iso", "--output", "o", "file:../files/a.tar"}, []string{root + "/files/a.tar"}
			},
		},
		{
			name: "image references need no mount",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--output", "o", "docker:img"}, nil
			},
		},
		{
			name: "oci and bare references need no mount",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/work/quay.io/kairos/")
				return []string{"build-iso", "--output", "o", "oci:img", "quay.io/kairos/alpine:3.21"}, nil
			},
		},
		{
			name: "stdin and URLs for cloud-config",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"--cloud-config", "-", "build-iso", "-c", "https://example.test/c.yaml", "--cloud-config=http://x/y", "--output", "o", "img"}, nil
			},
		},
		{
			name: "pkcs11 key",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-uki", "--tpm-pcr-private-key", "pkcs11:token=x;object=y", "img"}, nil
			},
		},
		{
			name: "directory symlink under cwd is mounted through its target",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/target/")
				if err := os.Symlink(root+"/target", root+"/work/link"); err != nil {
					t.Fatal(err)
				}
				return []string{"build-iso", "--output", "o", "--overlay-rootfs", "link", "img"}, []string{root + "/target"}
			},
		},
		{
			name: "file symlink under cwd is mounted through its target",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/target/c.yaml")
				if err := os.Symlink("../target/c.yaml", root+"/work/c.yaml"); err != nil {
					t.Fatal(err)
				}
				return []string{"build-iso", "--output", "o", "--cloud-config", "c.yaml", "img"}, []string{root + "/target/c.yaml"}
			},
		},
		{
			name: "unpack destination after a value flag",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/amd64/")
				return []string{"unpack", "--arch", "amd64", "quay.io/kairos/alpine:3.21", "../dest"}, []string{root + "/dest"}
			},
			created: map[string]bool{"dest": true},
		},
		{
			name: "netboot positionals",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/x.iso")
				return []string{"netboot", "../x.iso", "../netout"}, []string{root + "/netout", root + "/x.iso"}
			},
			created: map[string]bool{"netout": true},
		},
		{
			name: "table flag at a reserved path stops the run",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--output", "/etc/x", "img"}, nil
			},
			exit: 2,
		},
		{
			name: "table input at a reserved path stops the run",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--cloud-config", "/etc/passwd", "img"}, nil
			},
			exit: 2,
		},
		{
			name: "generic token at a reserved path is skipped",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--output", "o", "/etc", "img"}, nil
			},
		},
		{
			name: "path with a colon stops the run",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--output", "../a:b", "img"}, nil
			},
			exit:    2,
			created: map[string]bool{"a:b": false},
		},
		{
			name: "global flag before the subcommand",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"--debug", "build-iso", "--output", "../out2", "img"}, []string{root + "/out2"}
			},
			created: map[string]bool{"out2": true},
		},
		{
			name: "top level config file",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/cfg/aurora.yaml")
				return []string{"../cfg/aurora.yaml"}, []string{root + "/cfg/aurora.yaml"}
			},
		},
		{
			name: "one mount covers a nested path and a repeat",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/shared/iso/")
				return []string{"build-iso", "--output", "o", "--overlay-rootfs", "../shared", "--overlay-iso", "../shared/iso", "--extensions-catalog", "../shared", "img"}, []string{root + "/shared"}
			},
		},
		{
			name: "set values are not scanned",
			setup: func(t *testing.T, root string) ([]string, []string) {
				mk(t, root+"/rootfs/")
				return []string{"--set", "../rootfs", "--set=x=../rootfs", "build-iso", "--output", "o", "img"}, nil
			},
		},
		{
			name: "awkward arguments pass through byte for byte",
			setup: func(t *testing.T, root string) ([]string, []string) {
				return []string{"build-iso", "--output", "o", "--set", "a=b c\nd", "", "  ", "*", "img"}, nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := tempDir(t)
			cwd := filepath.Join(root, "work")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			args, want := tc.setup(t, root)
			res := runShim(t, "linux", "docker", shimOpts{}, cwd, args...)
			if res.exit != tc.exit {
				t.Fatalf("exit = %d, want %d; stderr: %s", res.exit, tc.exit, res.stderr)
			}
			for rel, shouldExist := range tc.created {
				_, err := os.Stat(filepath.Join(root, rel))
				if (err == nil) != shouldExist {
					t.Errorf("%s exists = %v, want %v", rel, err == nil, shouldExist)
				}
			}
			if tc.exit != 0 {
				if res.argv != nil {
					t.Errorf("the runtime was called: %q", res.argv)
				}
				return
			}
			sort.Strings(want)
			if got := mountedPaths(res.argv, cwd); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("mounts = %q, want %q (argv %q)", got, want, res.argv)
			}
			tail := append([]string{ImageRef()}, args...)
			got := res.argv[len(res.argv)-len(tail):]
			if strings.Join(got, "\x00") != strings.Join(tail, "\x00") {
				t.Errorf("arguments changed: got %q, want %q", got, tail)
			}
		})
	}
}

// reservedFunc returns the is_reserved function out of the shim, for tests
// that exercise the list itself.
func reservedFunc(t *testing.T) string {
	t.Helper()
	start := strings.Index(shimTemplate, "is_reserved() {")
	if start < 0 {
		t.Fatal("is_reserved not found in the shim")
	}
	end := strings.Index(shimTemplate[start:], "\n}\n")
	if end < 0 {
		t.Fatal("end of is_reserved not found in the shim")
	}
	return "nl='\n'\n" + shimTemplate[start:start+end+3]
}

func TestShimReservedList(t *testing.T) {
	fn := reservedFunc(t)
	check := func(path string) bool {
		t.Helper()
		err := exec.Command("sh", "-c", fn+"\nis_reserved \"$1\"", "sh", path).Run()
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatal(err)
		}
		return err == nil
	}
	for _, p := range []string{
		"/", "/tmp", "/var", "/private", "/private/var", "/etc", "/etc/x", "/usr/bin", "/bin", "/lib64", "/var/run/docker", "/run/user", "/proc/1", "/sys", "/dev/null", "/boot", "/sbin", "/amd", "/arm/x", "/riscv64",
		"/private/etc", "/private/etc/ssl", "/private/tmp", "/private/var/run", "/private/var/run/x",
		"/System", "/System/Library", "/Library", "/Library/x", "/Applications", "/Applications/x.app",
		"//etc", "//private/etc", "//usr/bin", "/a:b", "/a,b", "/a\nb",
	} {
		if !check(p) {
			t.Errorf("%q is not reserved, want reserved", p)
		}
	}
	for _, p := range []string{
		"/tmp/work", "/private/tmp/work", "/private/var/folders/x", "/private/var/folders/ab/cd/T/x",
		"/Users/me", "/Users/me/work", "/Volumes/disk", "/home/me", "/opt/x", "/srv/x", "/var/lib/x",
	} {
		if check(p) {
			t.Errorf("%q is reserved, want allowed", p)
		}
	}
}

// symlinkTo makes dir/name a symlink to target and returns its path.
func symlinkTo(t *testing.T, dir, name, target string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// On macOS /etc is itself a link to /private/etc, so a system directory is
// reached through a link there. These use links of our own to the same
// places, in a working directory that is itself reached through a link.
func TestShimReservedThroughSymlinks(t *testing.T) {
	for _, target := range []string{"/etc", "/usr"} {
		if _, err := os.Stat(target); err != nil {
			continue
		}
		root := tempDir(t)
		link := symlinkTo(t, root, "sys", target)
		work := filepath.Join(root, "work")
		if err := os.Mkdir(work, 0o755); err != nil {
			t.Fatal(err)
		}
		viaLink := symlinkTo(t, root, "workln", work)

		// (a) as the working directory.
		res := runShim(t, "linux", "docker", shimOpts{}, link, "genkey")
		if res.exit != 2 || res.argv != nil {
			t.Errorf("cwd %s: exit=%d argv=%q, want exit 2 and no run", link, res.exit, res.argv)
		}
		// (b) as a table-flag input, from a plain and from a linked cwd.
		for _, cwd := range []string{work, viaLink} {
			res = runShim(t, "linux", "docker", shimOpts{}, cwd, "build-iso", "--cloud-config", link, "img")
			if res.exit != 2 || res.argv != nil {
				t.Errorf("cwd %s, input %s: exit=%d argv=%q, want exit 2 and no run", cwd, link, res.exit, res.argv)
			}
			// (c) as a generic token: skipped, nothing mounted.
			res = runShim(t, "linux", "docker", shimOpts{}, cwd, "build-iso", "--output", "o", link, "img")
			if res.exit != 0 {
				t.Errorf("cwd %s, token %s: exit=%d, want 0; stderr %s", cwd, link, res.exit, res.stderr)
				continue
			}
			if m := mountedPaths(res.argv, work); len(m) != 0 {
				t.Errorf("cwd %s, token %s: mounted %q, want nothing", cwd, link, m)
			}
		}
	}
}

// A link that sits directly in / and has a relative target, /bin -> usr/bin on
// most Linux hosts and /etc -> private/etc on macOS, used to resolve to
// //usr/bin: not on the reserved list, and mounted as such.
func TestShimNeverMountsADoubleSlashPath(t *testing.T) {
	var links []string
	for _, p := range []string{"/bin", "/sbin", "/lib", "/etc", "/tmp", "/var"} {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			links = append(links, p)
		}
	}
	if len(links) == 0 {
		t.Skip("no symlink directly under / to test with")
	}
	root := tempDir(t)
	cwd := filepath.Join(root, "work")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	noDouble := func(what string, argv []string) {
		t.Helper()
		for i, a := range argv {
			if i > 0 && argv[i-1] == "-v" && (strings.HasPrefix(a, "//") || strings.Contains(a, ":/"+"/")) {
				t.Errorf("%s: mount argument %q has a doubled slash", what, a)
			}
		}
	}
	for _, l := range links {
		res := runShim(t, "linux", "docker", shimOpts{}, cwd, "build-iso", "--output", "o", l, "img")
		noDouble("generic "+l, res.argv)
		if m := mountedPaths(res.argv, cwd); len(m) != 0 {
			t.Errorf("generic token %s: mounted %q, want nothing", l, m)
		}
		res = runShim(t, "linux", "docker", shimOpts{}, cwd, "build-iso", "--cloud-config", l, "img")
		noDouble("input "+l, res.argv)
		if l != "/tmp" && l != "/var" && (res.exit != 2 || res.argv != nil) {
			t.Errorf("input %s: exit=%d argv=%q, want exit 2 and no run", l, res.exit, res.argv)
		}
	}
	// And for an ordinary run, no mount argument ever starts with two slashes.
	other := tempDir(t)
	res := runShim(t, "linux", "docker", shimOpts{}, cwd, "build-iso", "--output", other+"/out", "dir:"+other, "img")
	noDouble("ordinary", res.argv)
}
