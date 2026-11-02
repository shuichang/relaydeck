package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
	"github.com/shuichang/relaydeck/internal/engine"
	"github.com/shuichang/relaydeck/internal/server"
)

// Reproduce a fresh panel where sing-box is installed afterwards. Exercise the
// authenticated API and actual child process instead of assuming that changing
// an enabled checkbox means the engine has started.
func TestEngineInstalledAfterPanelCanSaveStartAndStop(t *testing.T) {
	exerciseEngineActivation(t, false)
}

// This opt-in test downloads the pinned official release through the protected
// panel API and verifies real TCP/UDP traffic without restarting the panel.
func TestEngineOneClickOfficialDownloadAndActivation(t *testing.T) {
	if os.Getenv("RELAYDECK_TEST_DOWNLOAD") != "1" || runtime.GOOS != "linux" {
		t.Skip("set RELAYDECK_TEST_DOWNLOAD=1 on Linux to exercise the official download")
	}
	exerciseEngineActivation(t, true)
}

func exerciseEngineActivation(t *testing.T, download bool) {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv(singBoxEnv))
	if binary == "" {
		t.Skipf("set %s to the sing-box %s binary", singBoxEnv, requiredVersion)
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	version, err := runBoundedCommand(5*time.Second, binary, "version")
	if err != nil || !strings.Contains(version, requiredVersion) {
		t.Fatalf("expected sing-box %s: %s %v", requiredVersion, version, err)
	}
	dir := t.TempDir()
	lateBinary := filepath.Join(dir, "later-installed-sing-box")
	if runtime.GOOS == "windows" {
		lateBinary += ".exe"
	}
	if err := server.InitializeAuth(dir, "admin", "integration-admin-password-123", false); err != nil {
		t.Fatal(err)
	}
	engineBinary := lateBinary
	if download {
		// Simulate a fresh VPS while preserving the system sing-box used by
		// other services and tests.
		t.Setenv("PATH", t.TempDir())
		engineBinary = ""
	}
	panel, err := server.New(server.Options{DataDir: dir, Version: "integration", EngineBinary: engineBinary})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(panel.Close)
	endpoint := httptest.NewTLSServer(panel.Handler())
	t.Cleanup(endpoint.Close)
	client := endpoint.Client()
	client.Timeout = 20 * time.Second
	client.Jar, _ = cookiejar.New(nil)
	api := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var input io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.NewReader(raw)
		}
		request, err := http.NewRequest(method, endpoint.URL+path, input)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-RelayDeck-CSRF", "1")
		request.Header.Set("Origin", endpoint.URL)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, raw
	}
	engineState := func() engine.Snapshot {
		t.Helper()
		status, raw := api(http.MethodGet, "/api/engine", nil)
		var state engine.Snapshot
		if status != http.StatusOK || json.Unmarshal(raw, &state) != nil {
			t.Fatalf("engine state: HTTP %d %s", status, raw)
		}
		return state
	}
	putEngine := func(enabled bool, raw json.RawMessage) engine.Snapshot {
		t.Helper()
		status, response := api(http.MethodPut, "/api/engine", config.Engine{Enabled: enabled, Config: raw})
		var state engine.Snapshot
		if status != http.StatusOK || json.Unmarshal(response, &state) != nil {
			t.Fatalf("save enabled=%t: HTTP %d %s", enabled, status, response)
		}
		return state
	}
	if status, _ := api(http.MethodPost, "/api/login", map[string]string{
		"username": "admin", "password": "integration-admin-password-123",
	}); status != http.StatusOK {
		t.Fatalf("login: HTTP %d", status)
	}
	if state := engineState(); state.Available || state.Running || state.Enabled {
		t.Fatalf("missing engine reported available or started: %+v", state)
	}
	_, before := api(http.MethodGet, "/api/config/export", nil)
	status, _ := api(http.MethodPut, "/api/engine", config.Engine{Enabled: true, Config: json.RawMessage(`{}`)})
	if status != http.StatusBadRequest {
		t.Fatalf("missing binary should prevent enabling: HTTP %d", status)
	}
	_, after := api(http.MethodGet, "/api/config/export", nil)
	if !bytes.Equal(before, after) {
		t.Fatal("failed enable persisted an unusable engine configuration")
	}
	// No panel restart or manager reconstruction is allowed here.
	wait := 9 * time.Second
	if download {
		status, response := api(http.MethodPost, "/api/engine/install", map[string]any{})
		if status != http.StatusAccepted {
			t.Fatalf("one-click install: HTTP %d %s", status, response)
		}
		wait = 5 * time.Minute
	} else {
		source, err := os.Open(binary)
		if err != nil {
			t.Fatal(err)
		}
		destination, err := os.OpenFile(lateBinary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			_ = source.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(destination, source)
		closeErr := destination.Close()
		_ = source.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("install fixture: copy=%v close=%v", copyErr, closeErr)
		}
	}
	deadline := time.Now().Add(wait)
	detected := false
	for time.Now().Before(deadline) {
		state := engineState()
		if state.Installation.Phase == "failed" {
			t.Fatalf("one-click installation failed: %s", state.Installation.Error)
		}
		if state.Available && (!download || state.Installation.Phase == "complete") {
			if state.Enabled || state.Running {
				t.Fatal("installation must not auto-enable a configuration")
			}
			detected = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !detected {
		t.Fatal("the running panel did not discover the later-installed engine")
	}
	if download {
		status, _ := api(http.MethodPost, "/api/engine/install", map[string]any{})
		if status != http.StatusConflict {
			t.Fatalf("already installed engine accepted another download: HTTP %d", status)
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "installed-engine-real-tcp")
	}))
	t.Cleanup(target.Close)
	targetHost, targetPortText, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	targetPort, _ := net.LookupPort("tcp", targetPortText)
	udpTarget, err := net.ListenPacket("udp4", net.JoinHostPort(targetHost, targetPortText))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udpTarget.Close() })
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, peer, err := udpTarget.ReadFrom(buffer)
			if err != nil {
				return
			}
			_, _ = udpTarget.WriteTo(buffer[:n], peer)
		}
	}()
	reserved, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reserved.Close() })
	coreAddress := reserved.Addr().String()
	corePort := reserved.Addr().(*net.TCPAddr).Port
	var template map[string]any
	if err := json.Unmarshal(engine.Templates()[0].Config, &template); err != nil {
		t.Fatal(err)
	}
	template["inbounds"].([]any)[0].(map[string]any)["listen_port"] = corePort
	route := template["route"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	route["override_address"], route["override_port"] = targetHost, targetPort
	raw, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	if state := putEngine(false, raw); !state.Available || state.Enabled || state.Running {
		t.Fatalf("saving a disabled validated template started a process: %+v", state)
	}
	_ = reserved.Close()
	if state := putEngine(true, raw); !state.Available || !state.Enabled || !state.Running {
		t.Fatalf("save-and-enable did not start the real engine: %+v", state)
	}
	checkTCP := func() {
		t.Helper()
		transport := &http.Transport{}
		defer transport.CloseIdleConnections()
		probe := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := probe.Get("http://" + coreAddress + "/activation")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 256))
		if err != nil || string(body) != "installed-engine-real-tcp" {
			t.Fatalf("actual engine TCP forwarding: %q %v", body, err)
		}
	}
	checkTCP()
	udpClient, err := net.DialTimeout("udp4", coreAddress, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer udpClient.Close()
	_ = udpClient.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := udpClient.Write([]byte("installed-engine-real-udp")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 128)
	n, err := udpClient.Read(reply)
	if err != nil || string(reply[:n]) != "installed-engine-real-udp" {
		t.Fatalf("actual engine UDP forwarding: %q %v", reply[:n], err)
	}
	if state := putEngine(false, raw); state.Enabled || state.Running || !state.Available {
		t.Fatalf("stop did not retain the installed engine and disabled config: %+v", state)
	}
	if state := putEngine(true, raw); !state.Enabled || !state.Running {
		t.Fatalf("saved configuration could not be enabled again: %+v", state)
	}
	checkTCP()
}
