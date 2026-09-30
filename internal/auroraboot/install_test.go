package auroraboot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func shimContent(t *testing.T) []byte {
	t.Helper()
	b, err := RenderShim("docker", ImageRef(), "linux")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInstallShimCreatesDirAndMarksFile(t *testing.T) {
	home := tempDir(t)
	dir := ShimDir(home)
	path, created, err := InstallShim(dir, shimContent(t))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("createdDir = false for a directory that did not exist")
	}
	if want := filepath.Join(dir, "auroraboot"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
	if !IsManagedShim(path) {
		t.Error("the installed file is not recognised as a managed shim")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the shim", len(entries))
	}
	// A second install into the now existing directory does not claim it.
	if _, created, err := InstallShim(dir, shimContent(t)); err != nil || created {
		t.Errorf("second install: created=%v err=%v, want false and nil", created, err)
	}
}

func TestInstallShimRefusesForeignFile(t *testing.T) {
	dir := tempDir(t)
	target := filepath.Join(dir, "auroraboot")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := InstallShim(dir, shimContent(t))
	if !errors.Is(err, ErrNotManaged) {
		t.Fatalf("err = %v, want ErrNotManaged", err)
	}
	b, _ := os.ReadFile(target)
	if string(b) != "#!/bin/sh\necho mine\n" {
		t.Errorf("the foreign file was modified: %q", b)
	}
	if _, _, err := InstallShim(dir, []byte("#!/bin/sh\n")); err == nil {
		t.Error("content without a marker was accepted")
	}
}

func TestInstallShimRewritesOwnFile(t *testing.T) {
	dir := tempDir(t)
	if _, _, err := InstallShim(dir, shimContent(t)); err != nil {
		t.Fatal(err)
	}
	next := append(shimContent(t), []byte("# newer\n")...)
	path, created, err := InstallShim(dir, next)
	if err != nil || created {
		t.Fatalf("rewrite: created=%v err=%v", created, err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(next) {
		t.Error("the shim was not rewritten")
	}
}

func TestRemoveShimLeavesForeignFile(t *testing.T) {
	dir := tempDir(t)

	noMarker := filepath.Join(dir, "plain")
	if err := os.WriteFile(noMarker, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveShim(noMarker); !errors.Is(err, ErrNotManaged) {
		t.Errorf("no marker: err = %v, want ErrNotManaged", err)
	}
	if _, err := os.Lstat(noMarker); err != nil {
		t.Errorf("the file without a marker was removed: %v", err)
	}

	// The marker as part of a longer line is not the marker line.
	partial := filepath.Join(dir, "partial")
	if err := os.WriteFile(partial, []byte("echo "+ShimMarker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveShim(partial); !errors.Is(err, ErrNotManaged) {
		t.Errorf("partial marker: err = %v, want ErrNotManaged", err)
	}

	// A symlink to a managed shim is not itself a managed shim.
	managed, _, err := InstallShim(filepath.Join(dir, "managed"), shimContent(t))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(managed, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveShim(link); !errors.Is(err, ErrNotManaged) {
		t.Errorf("symlink: err = %v, want ErrNotManaged", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink was removed: %v", err)
	}
	if !IsManagedShim(managed) {
		t.Error("the symlink target was damaged")
	}

	// A directory is not a shim either.
	if err := RemoveShim(filepath.Join(dir, "managed")); !errors.Is(err, ErrNotManaged) {
		t.Errorf("directory: err = %v, want ErrNotManaged", err)
	}

	// Its own file goes, and a missing one is not an error.
	if err := RemoveShim(managed); err != nil {
		t.Errorf("removing a managed shim: %v", err)
	}
	if err := RemoveShim(managed); err != nil {
		t.Errorf("removing a missing shim: %v", err)
	}
}

func TestRemoveShimDirOnlyWhenEmptyAndExpected(t *testing.T) {
	home := tempDir(t)
	dir := ShimDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveShimDirIfEmpty(dir, home); removed || err != nil {
		t.Errorf("non-empty dir: removed=%v err=%v, want false and nil", removed, err)
	}
	if err := os.Remove(filepath.Join(dir, "other")); err != nil {
		t.Fatal(err)
	}

	// An empty directory that is not the expected one stays.
	elsewhere := filepath.Join(home, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveShimDirIfEmpty(elsewhere, home); removed || err != nil {
		t.Errorf("unexpected dir: removed=%v err=%v, want false and nil", removed, err)
	}
	if removed, err := RemoveShimDirIfEmpty(dir, ""); removed || err != nil {
		t.Errorf("empty home: removed=%v err=%v, want false and nil", removed, err)
	}

	if removed, err := RemoveShimDirIfEmpty(dir, home); !removed || err != nil {
		t.Errorf("empty expected dir: removed=%v err=%v, want true and nil", removed, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory is still there: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local")); err != nil {
		t.Errorf("the parent .local was touched: %v", err)
	}
	if removed, err := RemoveShimDirIfEmpty(dir, home); removed || err != nil {
		t.Errorf("missing dir: removed=%v err=%v, want false and nil", removed, err)
	}

	// A symlink at the expected path is never followed.
	target := filepath.Join(home, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveShimDirIfEmpty(dir, home); removed || err != nil {
		t.Errorf("symlinked dir: removed=%v err=%v, want false and nil", removed, err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("the symlink target was removed: %v", err)
	}
}
