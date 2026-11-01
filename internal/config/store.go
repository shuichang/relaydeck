package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const MaxFileSize = 4 << 20

type Store struct {
	mu   sync.Mutex
	path string
}

func NewStore(dir string) *Store { return &Store{path: filepath.Join(dir, "config.json")} }

func Default() Config {
	return Config{Version: 1, Rules: []Rule{}, Engine: Engine{Config: json.RawMessage(`{}`)}}
}

func (s *Store) Load() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Default()
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if info.Size() > MaxFileSize {
		return c, fmt.Errorf("配置文件超过 %d 字节", MaxFileSize)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("解析配置失败: %w", err)
	}
	if c.Version != 1 {
		return c, fmt.Errorf("不支持配置版本 %d", c.Version)
	}
	if c.Rules == nil {
		c.Rules = []Rule{}
	}
	if len(c.Engine.Config) == 0 {
		c.Engine.Config = json.RawMessage(`{}`)
	}
	return c, nil
}

func (s *Store) Save(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Version = 1
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivate(s.path, append(b, '\n'))
}

// WritePrivate replaces a file only after the entire new content has been synced.
func WritePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".relaydeck-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// Sync the directory where supported, so a completed update survives power loss.
	if dir, openErr := os.Open(filepath.Dir(path)); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
