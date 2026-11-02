package server

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/shuichang/relaydeck/internal/config"
	"github.com/shuichang/relaydeck/internal/forward"
)

func TestProbeAuthenticationValidationAndNoPersistence(t *testing.T) {
	s, h, c, dir := testServer(t)
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	rule := config.Rule{Network: "tcp", Listen: "127.0.0.1:21453", Target: target.Addr().String()}
	payload, _ := json.Marshal(map[string]any{"rule": rule})
	route := h.URL + "/api/rules/probe"
	resp, _ := request(t, c, "POST", route, string(payload), h.URL, true)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated probe accepted: %d", resp.StatusCode)
	}
	login(t, h, c)
	resp, _ = request(t, c, "POST", route, string(payload), h.URL, false)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("probe bypassed CSRF checks")
	}
	resp, _ = request(t, c, "POST", route, string(payload), "https://attacker.invalid", true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("cross-origin probe was accepted")
	}
	if err := config.NewStore(dir).Save(config.Default()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	resp, raw := request(t, c, "POST", route, string(payload), h.URL, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe failed: %d %s", resp.StatusCode, raw)
	}
	var result forward.ProbeResult
	if err := json.Unmarshal(raw, &result); err != nil || result.Target.Status != "reachable" || result.CheckedAt.IsZero() {
		t.Fatalf("bad probe response: %s %v", raw, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !bytes.Equal(before, after) || len(s.ruleSnapshots()) != 0 {
		t.Fatal("probe persisted or applied a draft")
	}
	resp, _ = request(t, c, "POST", route, `{"rule":{"network":"tcp","listen":":0","target":"127.0.0.1:80"}}`, h.URL, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("invalid draft accepted")
	}
	resp, _ = request(t, c, "POST", route, `{"rule":{},"unknown":true}`, h.URL, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("unknown fields accepted")
	}
	for i := 0; i < cap(s.probes); i++ {
		s.probes <- struct{}{}
	}
	resp, _ = request(t, c, "POST", route, string(payload), h.URL, true)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatal("probe concurrency limit ignored")
	}
	for i := 0; i < cap(s.probes); i++ {
		<-s.probes
	}
}

func TestRuleDefaultConnectionsAndExplicitLimitPreserved(t *testing.T) {
	_, h, c, dir := testServer(t)
	login(t, h, c)
	payload := `{"rules":[{"id":"default","network":"tcp","listen":"127.0.0.1:21451","target":"127.0.0.1:80","max_connections":0},{"id":"explicit","network":"tcp","listen":"127.0.0.1:21452","target":"127.0.0.1:80","max_connections":256}]}`
	resp, raw := request(t, c, "PUT", h.URL+"/api/rules", payload, h.URL, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save limits: %d %s", resp.StatusCode, raw)
	}
	cfg, err := config.NewStore(dir).Load()
	if err != nil || cfg.Rules[0].MaxConnections != config.DefaultMaxConnections || cfg.Rules[1].MaxConnections != 256 {
		t.Fatalf("default or explicit limit corrupted: %+v %v", cfg.Rules, err)
	}
}
