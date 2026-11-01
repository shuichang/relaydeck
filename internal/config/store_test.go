package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrivateAtomicStore(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	c, err := s.Load()
	if err != nil || c.Version != 1 || c.Rules == nil {
		t.Fatal("missing config must produce safe defaults", err)
	}
	c.Rules = []Rule{{ID: "a", Name: "a"}}
	if err = s.Save(c); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load()
	if err != nil || len(loaded.Rules) != 1 || loaded.Rules[0].ID != "a" {
		t.Fatal("round trip failed", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "config.json"))
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("config containing node secrets must be owner-only")
	}
	if err = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"version":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(); err == nil {
		t.Fatal("unsupported version accepted")
	}
}
