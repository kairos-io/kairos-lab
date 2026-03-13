//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

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

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, string(out))
		}
	}

	defer func() {
		_ = exec.Command(bin, "implode", "--yes").Run()
	}()

	run("setup", "--yes")
	run("start", "--url", isoURL, "--yes")
	time.Sleep(5 * time.Second)
	run("status")
	run("stop")
	run("reset")
	run("implode", "--yes")
}
