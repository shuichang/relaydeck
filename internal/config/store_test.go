package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestFormattingCannotCreateAnUnloadableConfiguration(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	old := Default()
	if err := store.Save(old); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	before, _ := os.ReadFile(path)
	large := Default()
	// Compact JSON is under 1 MiB, but indentation expands it beyond 4 MiB.
	large.Engine.Config = json.RawMessage(`{"values":[` + strings.Repeat(`0,`, 400000) + `0]}`)
	if len(large.Engine.Config) >= MaxFileSize {
		t.Fatal("test requires a small input which expands during serialization")
	}
	if err := store.Save(large); err == nil {
		t.Fatal("an expanded configuration exceeded the load limit but was saved")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("size rejection modified the previous file")
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("previous configuration must remain loadable: %v", err)
	}
}
