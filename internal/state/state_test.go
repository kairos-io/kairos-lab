package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsDefault(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != SchemaVersion {
		t.Fatalf("unexpected version: %d", st.Version)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("KAIROS_LAB_CONFIG_DIR", t.TempDir())
	t.Setenv("KAIROS_LAB_CACHE_DIR", t.TempDir())
	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(store)
	st.Platform.OS = "linux"
	st.Setup.InstalledByKairosLab = []string{"qemu"}
	stateFile := filepath.Join(store.ConfigDir, "state.json")
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Platform.OS != "linux" {
		t.Fatalf("platform mismatch: %s", loaded.Platform.OS)
	}
	if len(loaded.Setup.InstalledByKairosLab) != 1 || loaded.Setup.InstalledByKairosLab[0] != "qemu" {
		t.Fatalf("dependencies mismatch: %#v", loaded.Setup.InstalledByKairosLab)
	}
}
