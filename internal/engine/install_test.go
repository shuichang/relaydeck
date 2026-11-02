package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type installTransport func(*http.Request) (*http.Response, error)

func (f installTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testArchive(t *testing.T, member string, kind byte, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	size := int64(len(payload))
	if kind == tar.TypeSymlink {
		size = 0
	}
	if err := tw.WriteHeader(&tar.Header{Name: member, Typeflag: kind, Size: size, Mode: 0755, Linkname: "/tmp/untrusted"}); err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func archiveDigest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func TestOfficialInstallRedirectPolicy(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"https://github.com/SagerNet/sing-box/releases/download/v1.14.4/x", true},
		{"https://release-assets.githubusercontent.com/path?secret=signature", true},
		{"https://objects.githubusercontent.com/path", true},
		{"http://github.com/path", false},
		{"https://github.com.attacker.invalid/path", false},
		{"https://127.0.0.1/path", false},
		{"https://github.com:444/path", false},
		{"https://user@github.com/path", false},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := officialRedirect(&http.Request{URL: u}, nil) == nil; got != tc.ok {
			t.Errorf("redirect %q allowed=%t", tc.raw, got)
		}
	}
	u, _ := url.Parse("https://github.com/path")
	if officialRedirect(&http.Request{URL: u}, make([]*http.Request, 5)) == nil {
		t.Fatal("unbounded redirects")
	}
	safe := safeInstallError(&url.Error{Op: "Get", URL: "https://release-assets.githubusercontent.com/path?secret=signature", Err: errors.New("failure")})
	if strings.Contains(safe, "signature") || strings.Contains(safe, "https://") {
		t.Fatalf("signed URL exposed: %s", safe)
	}
}

func TestArchiveIntegrityAndExactExtraction(t *testing.T) {
	const member = "sing-box-1.14.4-linux-amd64/sing-box"
	for _, tc := range []struct {
		name, member string
		kind         byte
		ok           bool
	}{
		{"regular", member, tar.TypeReg, true},
		{"traversal", "../../sing-box", tar.TypeReg, false},
		{"wrong_member", "other/sing-box", tar.TypeReg, false},
		{"symlink", member, tar.TypeSymlink, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := testArchive(t, tc.member, tc.kind, []byte("fixture"))
			archive := filepath.Join(dir, "release.tar.gz")
			if err := os.WriteFile(archive, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyArchive(archive, archiveDigest(raw)); err != nil {
				t.Fatal(err)
			}
			if verifyArchive(archive, strings.Repeat("0", 64)) == nil {
				t.Fatal("incorrect SHA accepted")
			}
			target := filepath.Join(dir, "sing-box")
			if got := extractBinary(archive, member, target); (got == nil) != tc.ok {
				t.Fatalf("extract: %v", got)
			}
			if tc.ok {
				if extractBinary(archive, member, target) == nil {
					t.Fatal("existing file overwritten")
				}
				content, _ := os.ReadFile(target)
				if string(content) != "fixture" {
					t.Fatal("wrong extracted binary")
				}
			}
		})
	}
	for _, arch := range []string{"amd64", "arm64", "arm"} {
		a, ok := officialAsset("linux", arch)
		if !ok || len(a.digest) != 64 || !strings.HasPrefix(a.name, "sing-box-1.14.4-linux-") {
			t.Fatalf("invalid pinned asset: %+v", a)
		}
	}
	if _, ok := officialAsset("windows", "amd64"); ok {
		t.Fatal("unsupported platform accepted")
	}
}

func testInstallerManager(t *testing.T) *Manager {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only executable and private installation path")
	}
	t.Setenv("PATH", t.TempDir())
	m := New(t.TempDir(), "")
	t.Cleanup(m.Close)
	return m
}

func waitInstallation(t *testing.T, m *Manager) Installation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := m.installer.snapshot()
		if !state.Running {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("install job did not finish")
	return Installation{}
}

func TestInstallSingleJobIntegrityFailureAndRetry(t *testing.T) {
	m := testInstallerManager(t)
	i := m.installer
	raw := testArchive(t, i.asset.name+"/sing-box", tar.TypeReg, []byte("#!/bin/sh\nprintf 'sing-box version 1.14.4\\n'\n"))
	i.asset.digest = archiveDigest(raw)
	gate := make(chan struct{})
	var count atomic.Int32
	i.client = &http.Client{Transport: installTransport(func(r *http.Request) (*http.Response, error) {
		count.Add(1)
		if r.URL.Scheme != "https" || r.URL.Host != "github.com" || !strings.Contains(r.URL.Path, "/v1.14.4/") {
			t.Error("unexpected download URL")
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		payload := raw
		if count.Load() == 1 {
			payload = []byte("tampered")
		}
		return &http.Response{StatusCode: 200, ContentLength: int64(len(payload)), Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
	})}
	for n := 0; n < 8; n++ {
		state, err := m.StartInstall()
		if err != nil || !state.Running {
			t.Fatalf("start/duplicate: %+v %v", state, err)
		}
	}
	close(gate)
	state := waitInstallation(t, m)
	if state.Phase != "failed" || !strings.Contains(state.Error, "SHA-256") || count.Load() != 1 {
		t.Fatalf("integrity failure not handled: %+v requests=%d", state, count.Load())
	}
	if _, err := os.Stat(filepath.Join(m.dataDir, "engines", "sing-box")); !os.IsNotExist(err) {
		t.Fatal("failed verification installed a binary")
	}
	if _, err := m.StartInstall(); err != nil {
		t.Fatal(err)
	}
	state = waitInstallation(t, m)
	if state.Phase != "complete" || count.Load() != 2 {
		t.Fatalf("retry: %+v requests=%d", state, count.Load())
	}
	snapshot := m.Snapshot()
	if !snapshot.Available || snapshot.Enabled || snapshot.Running || string(snapshot.Config) != "{}" {
		t.Fatalf("install should discover binary without enabling config: %+v", snapshot)
	}
	if _, err := m.StartInstall(); err == nil || count.Load() != 2 {
		t.Fatal("already installed binary was downloaded again")
	}
	entries, _ := os.ReadDir(filepath.Join(m.dataDir, "engines"))
	if len(entries) != 1 || entries[0].Name() != "sing-box" {
		t.Fatalf("staging files leaked: %+v", entries)
	}
	// A newly started manager must discover the private path as well.
	reopened := New(m.dataDir, "")
	defer reopened.Close()
	if !reopened.Snapshot().Available {
		t.Fatal("private binary not discovered after restart")
	}
}

func TestInstallCloseCancelsAndWaits(t *testing.T) {
	m := testInstallerManager(t)
	began := make(chan struct{})
	m.installer.client = &http.Client{Transport: installTransport(func(r *http.Request) (*http.Response, error) {
		close(began)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	if _, err := m.StartInstall(); err != nil {
		t.Fatal(err)
	}
	<-began
	m.Close()
	if state := m.installer.snapshot(); state.Running || state.Phase != "failed" {
		t.Fatalf("close left job active: %+v", state)
	}
	entries, _ := os.ReadDir(filepath.Join(m.dataDir, "engines"))
	if len(entries) != 0 {
		t.Fatal("cancelled installation leaked files")
	}
	if _, err := m.StartInstall(); err == nil {
		t.Fatal("closed manager accepted another job")
	}
}

func TestInstallRejectsOversizedResponseAndCustomPath(t *testing.T) {
	m := testInstallerManager(t)
	m.installer.client = &http.Client{Transport: installTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: maxArchiveSize + 1, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	if _, err := m.StartInstall(); err != nil {
		t.Fatal(err)
	}
	if state := waitInstallation(t, m); state.Phase != "failed" || !strings.Contains(state.Error, "大小限制") {
		t.Fatalf("oversized response: %+v", state)
	}
	custom := New(t.TempDir(), "/missing/custom-sing-box")
	defer custom.Close()
	if state, err := custom.StartInstall(); err == nil || state.Supported {
		t.Fatal("custom binary path accepted managed installation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.installer.install(ctx); err == nil {
		t.Fatal("cancelled context accepted")
	}
}

func TestBrokenSystemBinaryFallsBackToPrivateCore(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux executable fixtures")
	}
	systemDir := t.TempDir()
	t.Setenv("PATH", systemDir)
	systemPath := filepath.Join(systemDir, "sing-box")
	broken := []byte("#!/bin/sh\nexit 1\n")
	if err := os.WriteFile(systemPath, broken, 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := New(dir, "")
	defer m.Close()
	if m.Snapshot().Available {
		t.Fatal("broken system binary reported available")
	}
	raw := testArchive(t, m.installer.asset.name+"/sing-box", tar.TypeReg, []byte("#!/bin/sh\nprintf 'sing-box version 1.14.4\\n'\n"))
	m.installer.asset.digest = archiveDigest(raw)
	m.installer.client = &http.Client{Transport: installTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(len(raw)), Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})}
	if _, err := m.StartInstall(); err != nil {
		t.Fatal(err)
	}
	if state := waitInstallation(t, m); state.Phase != "complete" {
		t.Fatalf("broken PATH binary masked private install: %+v", state)
	}
	if state := m.Snapshot(); !state.Available {
		t.Fatal("private binary not available")
	}
	if original, err := os.ReadFile(systemPath); err != nil || !bytes.Equal(original, broken) {
		t.Fatal("system binary modified")
	}
	validSystem := []byte("#!/bin/sh\nprintf 'sing-box version system\\n'\n")
	if err := os.WriteFile(systemPath, validSystem, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := m.ensureBinary(true)
	if err != nil || path != systemPath {
		t.Fatalf("valid system binary lost priority: %q %v", path, err)
	}
}
