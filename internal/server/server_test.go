package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shuichang/relaydeck/internal/config"
)

func testServer(t *testing.T) (*Server, *httptest.Server, *http.Client, string) {
	t.Helper()
	dir := t.TempDir()
	if err := InitializeAuth(dir, "admin", "valid-test-password-123", false); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{DataDir: dir, Version: "test", EngineBinary: filepath.Join(dir, "missing-engine")})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewTLSServer(s.Handler())
	client := h.Client()
	client.Jar, _ = cookiejar.New(nil)
	t.Cleanup(func() { h.Close(); s.Close() })
	return s, h, client, dir
}

func request(t *testing.T, client *http.Client, method, url, body, origin string, csrf bool) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-RelayDeck-CSRF", "1")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func login(t *testing.T, h *httptest.Server, c *http.Client) {
	t.Helper()
	resp, b := request(t, c, "POST", h.URL+"/api/login", `{"username":"admin","password":"valid-test-password-123"}`, h.URL, true)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie must be Secure, HttpOnly and SameSite Strict")
	}
}

func TestAuthenticationAndCSRF(t *testing.T) {
	_, h, c, _ := testServer(t)
	resp, _ := request(t, c, "GET", h.URL+"/api/status", "", "", false)
	if resp.StatusCode != 401 {
		t.Fatalf("unprotected status: %d", resp.StatusCode)
	}
	resp, _ = request(t, c, "POST", h.URL+"/api/login", `{"username":"admin","password":"valid-test-password-123"}`, h.URL, false)
	if resp.StatusCode != 403 {
		t.Fatal("missing CSRF header accepted")
	}
	resp, _ = request(t, c, "POST", h.URL+"/api/login", `{"username":"admin","password":"valid-test-password-123"}`, "https://attacker.invalid", true)
	if resp.StatusCode != 403 {
		t.Fatal("cross-origin login accepted")
	}
	login(t, h, c)
	resp, b := request(t, c, "GET", h.URL+"/api/status", "", "", false)
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte(`"version":"test"`)) {
		t.Fatalf("status: %d %s", resp.StatusCode, b)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive API response can be cached")
	}
	request(t, c, "POST", h.URL+"/api/logout", `{}`, h.URL, true)
	resp, _ = request(t, c, "GET", h.URL+"/api/rules", "", "", false)
	if resp.StatusCode != 401 {
		t.Fatal("logout did not invalidate session")
	}
}

func TestRulePersistenceAndDiskFailureRollback(t *testing.T) {
	s, h, c, dir := testServer(t)
	login(t, h, c)
	body := `{"rules":[{"name":"disabled","enabled":false,"network":"both","listen":"127.0.0.1:21453","target":"192.0.2.1:443","max_connections":16,"udp_timeout":30}]}`
	resp, b := request(t, c, "PUT", h.URL+"/api/rules", body, h.URL, true)
	if resp.StatusCode != 200 {
		t.Fatalf("put rules: %d %s", resp.StatusCode, b)
	}
	stored, err := config.NewStore(dir).Load()
	if err != nil || len(stored.Rules) != 1 || stored.Rules[0].ID == "" {
		t.Fatalf("rules not persisted: %+v %v", stored, err)
	}
	if err := os.Remove(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "config.json"), 0700); err != nil {
		t.Fatal(err)
	}
	resp, b = request(t, c, "PUT", h.URL+"/api/rules", `{"rules":[]}`, h.URL, true)
	if resp.StatusCode != 500 {
		t.Fatalf("disk failure was hidden: %d %s", resp.StatusCode, b)
	}
	if rows := s.forward.Snapshot(); len(rows) != 1 || rows[0].ID != stored.Rules[0].ID {
		t.Fatal("runtime rules not rolled back after persistence failure")
	}
}

func TestPasswordChangeInvalidatesSession(t *testing.T) {
	_, h, c, dir := testServer(t)
	login(t, h, c)
	body := `{"old_password":"valid-test-password-123","new_password":"new-valid-password-456"}`
	resp, b := request(t, c, "POST", h.URL+"/api/password", body, h.URL, true)
	if resp.StatusCode != 200 {
		t.Fatalf("change password: %d %s", resp.StatusCode, b)
	}
	resp, _ = request(t, c, "GET", h.URL+"/api/status", "", "", false)
	if resp.StatusCode != 401 {
		t.Fatal("old session still active")
	}
	auth, err := LoadAuth(dir)
	if err != nil || !auth.Verify("admin", "new-valid-password-456") || auth.Verify("admin", "valid-test-password-123") {
		t.Fatal("password update not persisted correctly")
	}
	b, _ = os.ReadFile(filepath.Join(dir, "auth.json"))
	if strings.Contains(string(b), "new-valid-password-456") {
		t.Fatal("password stored in plaintext")
	}
}

func TestInvalidJSONAndLoginThrottle(t *testing.T) {
	_, h, c, _ := testServer(t)
	for i := 0; i < 5; i++ {
		resp, _ := request(t, c, "POST", h.URL+"/api/login", `{"username":"admin","password":"wrong"}`, h.URL, true)
		if resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := request(t, c, "POST", h.URL+"/api/login", `{"username":"admin","password":"valid-test-password-123"}`, h.URL, true)
	if resp.StatusCode != 429 {
		t.Fatal("password guessing was not throttled")
	}
}

func TestExportDoesNotContainAdminCredentials(t *testing.T) {
	_, h, c, _ := testServer(t)
	login(t, h, c)
	resp, b := request(t, c, "GET", h.URL+"/api/config/export", "", "", false)
	if resp.StatusCode != 200 || bytes.Contains(b, []byte("password")) || bytes.Contains(b, []byte(`"hash"`)) {
		t.Fatalf("unsafe export: %d", resp.StatusCode)
	}
	var cfg config.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
}
