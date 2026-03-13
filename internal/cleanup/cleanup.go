package cleanup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/kairos-io/kairos-lab/internal/state"
)

func IsPathSafe(path string, managedDirs []string) bool {
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, root := range managedDirs {
		rAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rAbs, abs)
		if err == nil && rel != ".." && rel != "." && rel != "" && rel != string(filepath.Separator) && !startsWithParent(rel) {
			return true
		}
		if abs == rAbs {
			return true
		}
	}
	return false
}

func startsWithParent(rel string) bool {
	if rel == ".." {
		return true
	}
	if len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return true
	}
	return false
}

func RemoveManagedFiles(st *state.State) error {
	for _, p := range st.ManagedFiles {
		if !IsPathSafe(p, st.ManagedDirs) {
			continue
		}
		_ = os.Remove(p)
	}
	return nil
}

func RemoveManagedDirs(st *state.State) error {
	dirs := append([]string{}, st.ManagedDirs...)
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if !IsPathSafe(d, st.ManagedDirs) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("remove directory %s: %w", d, err)
		}
	}
	return nil
}

func DependenciesToRemove(preExisting, installed []string) []string {
	pre := map[string]struct{}{}
	for _, n := range preExisting {
		pre[n] = struct{}{}
	}
	out := []string{}
	for _, n := range installed {
		if _, exists := pre[n]; exists {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
