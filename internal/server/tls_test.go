package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/shuichang/relaydeck/internal/paneltls"
)

func TestTLSSettingsAuthenticationValidationAndPrivateExport(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM, err := paneltls.GenerateSelfSigned([]string{"localhost", "127.0.0.1"}, 365)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "default.crt"), filepath.Join(dir, "default.key")
	if err = os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := paneltls.New(paneltls.Options{DataDir: dir, DefaultCertFile: certFile, DefaultKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	if err = InitializeAuth(dir, "admin", "valid-test-password-123", false); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{DataDir: dir, Version: "test", EngineBinary: filepath.Join(dir, "missing-engine"), TLS: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewTLSServer(s.Handler())
	defer h.Close()
	c := h.Client()
	c.Jar, _ = cookiejar.New(nil)
	for _, method := range []string{"GET", "PUT"} {
		resp, _ := request(t, c, method, h.URL+"/api/settings/tls", `{"mode":"default"}`, h.URL, true)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("TLS settings not protected: %s status=%d", method, resp.StatusCode)
		}
	}
	login(t, h, c)
	resp, _ := request(t, c, "PUT", h.URL+"/api/settings/tls", `{"mode":"default"}`, h.URL, false)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("TLS mutation without CSRF header was accepted")
	}
	before := manager.State().Certificate.FingerprintSHA256
	for _, body := range []string{
		`{"mode":"pem","certificate_pem":"invalid","private_key_pem":"invalid"}`,
		`{"mode":"self_signed","hosts":["https://invalid.example"],"days":365}`,
		`{"mode":"default","unknown":true}`,
		`{"mode":"default"} {}`,
	} {
		resp, _ = request(t, c, "PUT", h.URL+"/api/settings/tls", body, h.URL, true)
		if resp.StatusCode != http.StatusBadRequest || manager.State().Certificate.FingerprintSHA256 != before {
			t.Fatal("invalid TLS request replaced the active certificate")
		}
	}
	newCert, newKey, err := paneltls.GenerateSelfSigned([]string{"panel.example.com", "127.0.0.1"}, 365)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(paneltls.Update{Mode: "pem", CertificatePEM: string(newCert), PrivateKeyPEM: string(newKey)})
	resp, body := request(t, c, "PUT", h.URL+"/api/settings/tls", string(payload), h.URL, true)
	if resp.StatusCode != http.StatusOK || manager.State().Certificate.FingerprintSHA256 == before {
		t.Fatalf("valid certificate was not saved: status=%d body=%s", resp.StatusCode, body)
	}
	for _, route := range []string{"/api/settings/tls", "/api/config/export", "/api/status"} {
		resp, body = request(t, c, "GET", h.URL+route, "", "", false)
		if resp.StatusCode != 200 {
			t.Fatal("cannot inspect settings after certificate update", route, resp.StatusCode)
		}
		if bytes.Contains(body, newKey) || bytes.Contains(body, []byte("PRIVATE KEY")) ||
			bytes.Contains(body, []byte("private_key_pem")) || bytes.Contains(body, []byte("certificate_pem")) {
			t.Fatal("private TLS settings leaked into a response", route)
		}
	}
	resp, _ = request(t, c, "POST", h.URL+"/api/settings/tls/reload", `{"ignored":true}`, h.URL, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("reload accepted nonempty object")
	}
	resp, _ = request(t, c, "PUT", h.URL+"/api/settings/tls", `{"mode":"self_signed","hosts":["localhost","::1"],"days":30}`, h.URL, true)
	if resp.StatusCode != http.StatusOK || manager.State().Mode != "self_signed" {
		t.Fatal("self-signed mode did not apply")
	}
	resp, _ = request(t, c, "PUT", h.URL+"/api/settings/tls", `{"mode":"default"}`, h.URL, true)
	if resp.StatusCode != http.StatusOK || manager.State().Certificate.FingerprintSHA256 != before {
		t.Fatal("restoring the startup default did not work")
	}
}

func TestLocalHTTPModeReportsTLSUnavailable(t *testing.T) {
	_, h, c, _ := testServer(t)
	login(t, h, c)
	resp, body := request(t, c, "GET", h.URL+"/api/settings/tls", "", "", false)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"available":false`)) {
		t.Fatalf("local mode TLS state: %d %s", resp.StatusCode, body)
	}
	resp, _ = request(t, c, "PUT", h.URL+"/api/settings/tls", `{"mode":"default"}`, h.URL, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatal("local HTTP mode accepted TLS mutations")
	}
}
