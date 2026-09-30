//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// qemuMonitorQuit is what the CLI tells a user to press to exit a VM, and the
// only exit this CLI has: `start -display serial` runs QEMU with `-nographic
// -serial mon:stdio` (internal/vm), so Ctrl-a followed by x on the start
// process's stdin makes QEMU quit. There is no `stop` subcommand -- reset and
// cleanup both refuse while a VM is live and point at this same key sequence.
const qemuMonitorQuit = "\x01x"

// TestLifecycleLinux drives setup, start, status, exit, reset and cleanup
// through the binary, using the verbs and flags the CLI actually declares.
//
// Two things about the CLI shape this test:
//
//   - `start` is a foreground command. runStart ends in command.Wait(), which
//     blocks for the whole life of the VM, so it cannot be called through a
//     helper that waits for the process to exit -- doing that hangs the test
//     instead of failing it. It runs as a background process here, with its
//     own stdin pipe, and the VM is exited by writing to that pipe.
//   - There is no URL input anywhere. `start` takes `-iso <path>`, a local
//     file, and `download` selects from the published releases interactively.
//     So ISO_URL is fetched by this test and handed over as a path.
func TestLifecycleLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux e2e only")
	}
	if os.Getenv("KAIROS_LAB_E2E") != "1" {
		t.Skip("set KAIROS_LAB_E2E=1 to run")
	}
	isoURL := os.Getenv("ISO_URL")
	if isoURL == "" {
		t.Skip("set ISO_URL to a downloadable Kairos ISO")
	}

	bin := os.Getenv("KAIROS_LAB_BIN")
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "kairos-lab")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/kairos-lab")
		cmd.Dir = repoRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build binary: %v\n%s", err, string(out))
		}
	}

	configRoot := t.TempDir()
	cacheRoot := t.TempDir()
	env := append(os.Environ(),
		"KAIROS_LAB_CONFIG_DIR="+configRoot,
		"KAIROS_LAB_CACHE_DIR="+cacheRoot,
	)

	// run is for the commands that return on their own. `start` is not one of
	// them; see the comment on this test.
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, string(out))
		}
		return string(out)
	}

	isoPath := fetchISO(t, isoURL)

	// -yes on every destructive verb: each one prompts without it, and reset
	// in particular used to be called here with no -yes at all, so it sat
	// waiting for a confirmation that nothing was going to type.
	run("setup", "-yes")

	// -display serial, because the default is window: `-display default`
	// needs an X display the runner does not have, and serial is also what
	// puts the QEMU monitor on stdin so the VM can be exited below.
	start := exec.Command(bin, "start", "-iso", isoPath, "-display", "serial", "-yes")
	start.Env = env
	startIn, err := start.StdinPipe()
	if err != nil {
		t.Fatalf("pipe start stdin: %v", err)
	}
	// Written by the process and read by the assertions below while it is
	// still running, so the buffer is guarded.
	startOut := &syncBuffer{}
	start.Stdout = startOut
	start.Stderr = startOut
	if err := start.Start(); err != nil {
		t.Fatalf("start vm: %v", err)
	}
	startExit := make(chan error, 1)
	go func() { startExit <- start.Wait() }()

	// A VM left running fails reset and cleanup, so a test that gives up
	// half way has to take it down rather than leak it onto the runner. Both
	// calls are best effort: the assertions below are what reports failure.
	stopped := false
	defer func() {
		if !stopped {
			_, _ = io.WriteString(startIn, qemuMonitorQuit)
			select {
			case <-startExit:
			case <-time.After(30 * time.Second):
				_ = start.Process.Kill()
			}
		}
		_ = exec.Command(bin, "cleanup", "-yes").Run()
	}()

	// Waited for rather than slept through: a fixed sleep either racks up
	// dead time or expires before QEMU is up, and `status` already reports
	// the fact this needs.
	waitFor(t, 3*time.Minute, "vm to come up", func() bool {
		return strings.Contains(run("status"), "vm running: true")
	}, startOut)

	if _, err := io.WriteString(startIn, qemuMonitorQuit); err != nil {
		t.Fatalf("write monitor quit to the serial console: %v", err)
	}
	select {
	case err := <-startExit:
		stopped = true
		if err != nil {
			t.Fatalf("start exited with an error after the VM was told to quit: %v\n%s", err, startOut.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("the VM did not exit on the key sequence the CLI documents (Ctrl-a x)\n%s", startOut.String())
	}

	// reset and cleanup both refuse while a VM is live, so the record has to
	// show it down before either is called. start clears the PID on the way
	// out, which is what this reads.
	waitFor(t, time.Minute, "vm to be recorded as down", func() bool {
		return strings.Contains(run("status"), "vm running: false")
	}, startOut)

	run("reset", "-yes")
	run("cleanup", "-yes")
}

// fetchISO downloads rawURL into a temporary file and returns its path.
//
// The name ends in .iso because iso.ResolveForStart rejects a -iso argument
// that does not.
func fetchISO(t *testing.T, rawURL string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kairos.iso")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create iso file: %v", err)
	}
	defer f.Close()

	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("get %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: unexpected status %s", rawURL, resp.Status)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatalf("download %s: %v", rawURL, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close iso file: %v", err)
	}
	return path
}

// waitFor polls cond until it holds, and fails with the VM's own output when
// it does not -- a bare "timed out" says nothing about why QEMU never came up.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool, vmOut *syncBuffer) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s\n%s", timeout, what, vmOut.String())
}

// repoRoot walks up from this file's directory to the module root, so the
// fallback `go build ./cmd/kairos-lab` runs from where that path resolves
// whatever directory `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("--- start output ---\n%s", b.buf.String())
}
