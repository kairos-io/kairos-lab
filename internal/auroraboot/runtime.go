package auroraboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Package-level so tests can swap them, the way the rest of kairos-lab does
// for host lookups.
var (
	execCommand = exec.CommandContext
	lookPath    = exec.LookPath
)

// runtimeProbeTimeout bounds `<runtime> info` and `<runtime> --version`: with
// a daemon that accepts connections and never answers, info would otherwise
// hang setup.
const runtimeProbeTimeout = 30 * time.Second

// runtimeOrder is the order DetectRuntime tries runtimes in when the user
// states no preference: Docker is the default.
var runtimeOrder = []string{"docker", "podman"}

// ValidRuntime reports whether name is a container runtime kairos-lab
// supports. The name ends up as an executed binary, so anything read back from
// state.json passes through here first.
func ValidRuntime(name string) bool {
	return name == "docker" || name == "podman"
}

// DetectRuntime picks the container runtime the shim will run on. A runtime is
// usable when `<runtime> info` exits 0. With prefer set, only that runtime is
// considered; otherwise the first usable one wins, docker before podman. A
// docker binary whose --version output names podman is podman under another
// name and does not count as docker.
//
// present lists the runtime binaries found on PATH, docker first, whether or
// not they work. When name is empty, reason says why no runtime was chosen, so
// the caller can tell "none installed" (present is empty) from "installed but
// not working" and never installs a runtime on top of a broken one.
func DetectRuntime(prefer string) (name string, present []string, reason string) {
	for _, rt := range runtimeOrder {
		if _, err := lookPath(rt); err == nil {
			present = append(present, rt)
		}
	}
	if prefer != "" && !ValidRuntime(prefer) {
		return "", present, fmt.Sprintf("%q is not a supported container runtime (use docker or podman)", prefer)
	}
	candidates := runtimeOrder
	if prefer != "" {
		candidates = []string{prefer}
	}
	var problems []string
	for _, rt := range candidates {
		if _, err := lookPath(rt); err != nil {
			problems = append(problems, rt+" is not installed")
			continue
		}
		if rt == "docker" && dockerIsPodman() {
			problems = append(problems, "docker is podman under another name")
			continue
		}
		if err := probe(rt, "info"); err != nil {
			problems = append(problems, fmt.Sprintf("%s is installed but `%s info` failed (%v)", rt, rt, err))
			continue
		}
		return rt, present, ""
	}
	if len(present) == 0 && prefer == "" {
		return "", present, "no container runtime is installed"
	}
	return "", present, strings.Join(problems, "; ")
}

func probe(rt string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeProbeTimeout)
	defer cancel()
	return execCommand(ctx, rt, args...).Run()
}

func dockerIsPodman() bool {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeProbeTimeout)
	defer cancel()
	out, err := execCommand(ctx, "docker", "--version").CombinedOutput()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "podman")
}

func checkRuntimeAndImage(rt, ref string) error {
	if !ValidRuntime(rt) {
		return fmt.Errorf("unsupported container runtime %q", rt)
	}
	if !ValidImageRef(ref) {
		return fmt.Errorf("unsupported image reference %q", ref)
	}
	return nil
}

// ImageExists reports whether ref is already in the runtime's local image
// store. A runtime that answers "no such image" is a false with no error; only
// a failure to run the runtime at all is an error.
func ImageExists(ctx context.Context, rt, ref string) (bool, error) {
	if err := checkRuntimeAndImage(rt, ref); err != nil {
		return false, err
	}
	err := execCommand(ctx, rt, "image", "inspect", ref).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, fmt.Errorf("run %s: %w", rt, err)
}

// PullImage pulls the full reference ref, sending the runtime's output to out.
func PullImage(ctx context.Context, rt, ref string, out io.Writer) error {
	if err := checkRuntimeAndImage(rt, ref); err != nil {
		return err
	}
	cmd := execCommand(ctx, rt, "pull", ref)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s pull %s: %w", rt, ref, err)
	}
	return nil
}

// RemoveImage removes the full reference ref, sending the runtime's output to
// out.
func RemoveImage(ctx context.Context, rt, ref string, out io.Writer) error {
	if err := checkRuntimeAndImage(rt, ref); err != nil {
		return err
	}
	cmd := execCommand(ctx, rt, "image", "rm", ref)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s image rm %s: %w", rt, ref, err)
	}
	return nil
}
