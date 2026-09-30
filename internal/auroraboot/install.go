package auroraboot

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ShimName is the file name of the installed command.
const ShimName = "auroraboot"

// maxShimSize bounds how much of a file IsManagedShim reads. The shim is a few
// kilobytes; a path in state.json that names something huge is not ours.
const maxShimSize = 1 << 20

// ErrNotManaged is returned when a path is not a shim kairos-lab wrote.
var ErrNotManaged = errors.New("not an auroraboot shim written by kairos-lab")

// ShimDir returns the directory the shim is installed into for a home
// directory.
func ShimDir(home string) string {
	return filepath.Join(home, ".local", "bin")
}

// InstallShim writes content as an executable file named auroraboot in dir,
// creating dir when it is missing, and reports whether it had to create dir.
// It refuses to replace a file that is not a shim kairos-lab wrote: only a
// regular file carrying the marker line is rewritten. The file is published by
// renaming a complete temporary file over the name.
func InstallShim(dir string, content []byte) (path string, createdDir bool, err error) {
	if !hasMarkerLine(content) {
		return "", false, errors.New("shim content lacks its marker line")
	}
	path = filepath.Join(dir, ShimName)
	if _, lerr := os.Lstat(path); lerr == nil {
		if !IsManagedShim(path) {
			return "", false, fmt.Errorf("%s exists and was not written by kairos-lab: %w", path, ErrNotManaged)
		}
	} else if !errors.Is(lerr, os.ErrNotExist) {
		return "", false, fmt.Errorf("inspect %s: %w", path, lerr)
	}
	if _, serr := os.Stat(dir); errors.Is(serr, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false, fmt.Errorf("create %s: %w", dir, err)
		}
		createdDir = true
	}
	tmp, err := os.CreateTemp(dir, ".auroraboot-*")
	if err != nil {
		return "", createdDir, fmt.Errorf("create temporary shim: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return "", createdDir, fmt.Errorf("write shim: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", createdDir, fmt.Errorf("set shim mode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", createdDir, fmt.Errorf("close shim: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", createdDir, fmt.Errorf("install shim: %w", err)
	}
	renamed = true
	return path, createdDir, nil
}

// IsManagedShim reports whether path is a regular file (not a symlink) that
// carries the marker line. Lstat, so a link is never followed.
func IsManagedShim(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxShimSize {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxShimSize))
	if err != nil {
		return false
	}
	return hasMarkerLine(b)
}

func hasMarkerLine(b []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), maxShimSize)
	for sc.Scan() {
		if sc.Text() == ShimMarker {
			return true
		}
	}
	return false
}

// RemoveShim removes the shim at path. It removes nothing, and returns
// ErrNotManaged, unless IsManagedShim holds. A path that is already gone is
// not an error.
func RemoveShim(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !IsManagedShim(path) {
		return fmt.Errorf("%s: %w", path, ErrNotManaged)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// RemoveShimDirIfEmpty removes dir, and reports whether it did, only when dir
// is exactly ShimDir(home), is a real directory and is empty. It uses os.Remove,
// which cannot remove a directory that has anything in it.
func RemoveShimDirIfEmpty(dir, home string) (bool, error) {
	if home == "" || dir != ShimDir(home) {
		return false, nil
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return false, nil
	}
	if err := os.Remove(dir); err != nil {
		return false, fmt.Errorf("remove %s: %w", dir, err)
	}
	return true, nil
}
