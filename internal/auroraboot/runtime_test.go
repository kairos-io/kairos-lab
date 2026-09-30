package auroraboot

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// fakeHost swaps lookPath and execCommand for the length of a test. binaries
// lists what is "on PATH"; behaviour maps "<binary> <args...>" to an exit code
// and, for --version, output. Every call is recorded.
type fakeHost struct {
	binaries map[string]bool
	exit     map[string]int
	output   map[string]string
	calls    []string
}

func newFakeHost(t *testing.T, binaries ...string) *fakeHost {
	t.Helper()
	h := &fakeHost{binaries: map[string]bool{}, exit: map[string]int{}, output: map[string]string{}}
	for _, b := range binaries {
		h.binaries[b] = true
	}
	oldLook, oldExec := lookPath, execCommand
	t.Cleanup(func() { lookPath, execCommand = oldLook, oldExec })
	lookPath = func(name string) (string, error) {
		if h.binaries[name] {
			return "/fake/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.Join(append([]string{name}, args...), " ")
		h.calls = append(h.calls, key)
		return exec.CommandContext(ctx, "sh", "-c", `printf '%s' "$1"; exit "$2"`, "sh", h.output[key], strconv.Itoa(h.exit[key]))
	}
	return h
}

func TestDetectRuntimePrefersUsableDocker(t *testing.T) {
	h := newFakeHost(t, "docker", "podman")
	h.output["docker --version"] = "Docker version 27.0.0, build abc"
	name, present, reason := DetectRuntime("")
	if name != "docker" || reason != "" {
		t.Fatalf("name = %q, reason = %q; want docker", name, reason)
	}
	if strings.Join(present, ",") != "docker,podman" {
		t.Errorf("present = %v", present)
	}

	// A docker that does not work falls through to a podman that does.
	h.exit["docker info"] = 1
	if name, _, _ := DetectRuntime(""); name != "podman" {
		t.Errorf("name = %q, want podman when docker info fails", name)
	}
}

func TestDetectRuntimeHonoursPreference(t *testing.T) {
	newFakeHost(t, "docker", "podman")
	if name, _, reason := DetectRuntime("podman"); name != "podman" || reason != "" {
		t.Errorf("name = %q, reason = %q; want podman", name, reason)
	}
	name, present, reason := DetectRuntime("bogus")
	if name != "" || reason == "" || len(present) != 2 {
		t.Errorf("unsupported preference: name=%q present=%v reason=%q", name, present, reason)
	}
	// Requesting a runtime that is not installed picks nothing, even though
	// another one works.
	newFakeHost(t, "docker")
	name, present, reason = DetectRuntime("podman")
	if name != "" || !strings.Contains(reason, "podman is not installed") || strings.Join(present, ",") != "docker" {
		t.Errorf("missing preference: name=%q present=%v reason=%q", name, present, reason)
	}
}

func TestDetectRuntimeTreatsPodmanDockerAsPodman(t *testing.T) {
	h := newFakeHost(t, "docker", "podman")
	h.output["docker --version"] = "Emulate Docker CLI using podman. Create /etc/containers/nodocker to quiet msg.\npodman version 5.2.0"
	name, _, reason := DetectRuntime("")
	if name != "podman" || reason != "" {
		t.Fatalf("name = %q, reason = %q; want podman", name, reason)
	}
	for _, c := range h.calls {
		if c == "docker info" {
			t.Errorf("docker info was run against a docker that is podman")
		}
	}
	name, _, reason = DetectRuntime("docker")
	if name != "" || !strings.Contains(reason, "podman") {
		t.Errorf("asking for docker: name=%q reason=%q, want none and a reason naming podman", name, reason)
	}
}

func TestDetectRuntimeNoUsable(t *testing.T) {
	h := newFakeHost(t, "docker", "podman")
	h.exit["docker info"] = 1
	h.exit["podman info"] = 125
	name, present, reason := DetectRuntime("")
	if name != "" || len(present) != 2 {
		t.Fatalf("name=%q present=%v, want no runtime and both present", name, present)
	}
	if !strings.Contains(reason, "docker info") || !strings.Contains(reason, "podman info") {
		t.Errorf("reason %q should say both info calls failed", reason)
	}
}

func TestDetectRuntimeNone(t *testing.T) {
	h := newFakeHost(t)
	name, present, reason := DetectRuntime("")
	if name != "" || len(present) != 0 || reason == "" {
		t.Errorf("name=%q present=%v reason=%q, want none present and a reason", name, present, reason)
	}
	if len(h.calls) != 0 {
		t.Errorf("commands ran with nothing installed: %v", h.calls)
	}
}

func TestPullAndRemoveUseFullRef(t *testing.T) {
	h := newFakeHost(t, "docker")
	ctx := context.Background()
	var out bytes.Buffer
	if err := PullImage(ctx, "docker", ImageRef(), &out); err != nil {
		t.Fatal(err)
	}
	if err := RemoveImage(ctx, "docker", ImageRef(), &out); err != nil {
		t.Fatal(err)
	}
	if ok, err := ImageExists(ctx, "docker", ImageRef()); !ok || err != nil {
		t.Errorf("ImageExists = %v, %v; want true", ok, err)
	}
	want := []string{
		"docker pull " + ImageRef(),
		"docker image rm " + ImageRef(),
		"docker image inspect " + ImageRef(),
	}
	if strings.Join(h.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %q, want %q", h.calls, want)
	}

	h.exit["docker image inspect "+ImageRef()] = 1
	if ok, err := ImageExists(ctx, "docker", ImageRef()); ok || err != nil {
		t.Errorf("a missing image: ImageExists = %v, %v; want false and no error", ok, err)
	}
	h.exit["docker pull "+ImageRef()] = 1
	if err := PullImage(ctx, "docker", ImageRef(), &out); err == nil {
		t.Error("a failing pull returned no error")
	}

	// Nothing unvalidated reaches a command line.
	before := len(h.calls)
	if err := PullImage(ctx, "/tmp/evil", ImageRef(), &out); err == nil {
		t.Error("an unsupported runtime was accepted")
	}
	if err := RemoveImage(ctx, "docker", "quay.io/kairos/auroraboot:v1 --all", &out); err == nil {
		t.Error("an unsupported reference was accepted")
	}
	if _, err := ImageExists(ctx, "docker", "alpine"); err == nil {
		t.Error("a foreign reference was accepted")
	}
	if len(h.calls) != before {
		t.Errorf("rejected inputs still ran commands: %v", h.calls[before:])
	}
}

func TestValidRuntime(t *testing.T) {
	for name, want := range map[string]bool{
		"docker":        true,
		"podman":        true,
		"":              false,
		"Docker":        false,
		"/tmp/evil":     false,
		"docker\n":      false,
		"podman --evil": false,
		"nerdctl":       false,
	} {
		if got := ValidRuntime(name); got != want {
			t.Errorf("ValidRuntime(%q) = %v, want %v", name, got, want)
		}
	}
}
