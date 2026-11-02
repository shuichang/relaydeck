package integration

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
	"github.com/shuichang/relaydeck/internal/forward"
)

const (
	singBoxEnv       = "RELAYDECK_SING_BOX"
	requiredVersion  = "1.14.4"
	processOutputMax = 64 << 10
)

type protocolCase struct {
	name           string
	network        string
	serverInbound  func(listenPort int) map[string]any
	clientOutbound func(server string, serverPort int) map[string]any
	secrets        []string
	serverTag      string
	clientProxyTag string
}

func TestSingBoxProtocols(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv(singBoxEnv))
	if binary == "" {
		t.Skipf("set %s to the sing-box %s binary", singBoxEnv, requiredVersion)
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		t.Fatalf("resolve sing-box path: %v", err)
	}
	versionOutput, err := runBoundedCommand(5*time.Second, absolute, "version")
	if err != nil {
		t.Fatalf("run sing-box version: %v: %s", err, versionOutput)
	}
	if !strings.Contains(versionOutput, requiredVersion) {
		t.Skipf("%s points to an incompatible sing-box: %s", singBoxEnv, firstLine(versionOutput))
	}

	dir := t.TempDir()
	certPath, keyPath, err := createSelfSignedCertificate(dir)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	decoy := startRealityDecoy(t)
	_, decoyPortText, err := net.SplitHostPort(decoy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	decoyPort, err := net.LookupPort("tcp", decoyPortText)
	if err != nil {
		t.Fatal(err)
	}
	realityPrivate, realityPublic, err := generateRealityKeyPair()
	if err != nil {
		t.Fatalf("generate Reality key pair: %v", err)
	}
	shortID, err := randomHex(4)
	if err != nil {
		t.Fatal(err)
	}
	uuid, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	hysteriaPassword, err := randomHex(24)
	if err != nil {
		t.Fatal(err)
	}

	protocols := []protocolCase{
		vlessCase(uuid),
		vlessRealityCase(uuid, realityPrivate, realityPublic, shortID, decoyPort),
		hysteria2Case(hysteriaPassword, certPath, keyPath),
	}

	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "relaydeck-e2e:"+r.URL.Query().Get("case"))
	}))
	t.Cleanup(httpTarget.Close)
	targetURL, err := url.Parse(httpTarget.URL)
	if err != nil {
		t.Fatal(err)
	}
	targetHost, targetPortText, err := net.SplitHostPort(targetURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	targetPort, err := net.LookupPort("tcp", targetPortText)
	if err != nil {
		t.Fatal(err)
	}

	for _, protocol := range protocols {
		protocol := protocol
		t.Run(protocol.name, func(t *testing.T) {
			serverPort := freePort(t, protocol.network, "127.0.0.1")
			serverConfig := buildServerConfig(protocol.serverTag, protocol.serverInbound(serverPort))
			server := startSingBox(t, absolute, serverConfig, protocol.secrets)
			if protocol.network == "tcp" {
				waitTCP(t, net.JoinHostPort("127.0.0.1", fmt.Sprint(serverPort)), server)
			} else {
				time.Sleep(300 * time.Millisecond)
				server.requireRunning(t)
			}

			relay4Port := freePort(t, protocol.network, "127.0.0.1")
			relay6Port := freePort(t, protocol.network, "::1")
			manager := startForwardManager(t, protocol.network, serverPort, relay4Port, relay6Port)

			paths := []struct {
				name      string
				relayHost string
				relayPort int
				ruleID    string
			}{
				{name: "ipv4-relay", relayHost: "127.0.0.1", relayPort: relay4Port, ruleID: "relay-v4"},
				{name: "ipv6-relay", relayHost: "::1", relayPort: relay6Port, ruleID: "relay-v6"},
			}
			for _, path := range paths {
				path := path
				t.Run(path.name, func(t *testing.T) {
					directPort := freePort(t, "tcp", "127.0.0.1")
					clientConfig := buildClientConfig(
						protocol.clientProxyTag,
						protocol.clientOutbound(path.relayHost, path.relayPort),
						directPort,
						targetHost,
						targetPort,
					)
					client := startSingBox(t, absolute, clientConfig, protocol.secrets)
					directAddress := net.JoinHostPort("127.0.0.1", fmt.Sprint(directPort))
					waitTCP(t, directAddress, client)

					want := "relaydeck-e2e:" + protocol.name
					requestThroughDirect(t, directAddress, want, client, server)

					// The peer can receive the response before the forwarding
					// goroutine resumes after Write and updates its counter.
					// Wait for accounting, without issuing another request.
					deadline := time.Now().Add(2 * time.Second)
					for time.Now().Before(deadline) {
						ready := false
						for _, row := range manager.Snapshot() {
							if row.ID == path.ruleID && row.BytesIn > 0 && row.BytesOut > 0 {
								ready = true
							}
						}
						if ready {
							break
						}
						time.Sleep(time.Millisecond)
					}
					snapshot := manager.Snapshot()
					var found bool
					for _, row := range snapshot {
						if row.ID == path.ruleID {
							found = true
							if !row.Running || row.BytesIn == 0 || row.BytesOut == 0 {
								t.Fatalf("relay %s did not carry bidirectional bytes: %+v", path.ruleID, row)
							}
						}
					}
					if !found {
						t.Fatalf("missing relay snapshot %q", path.ruleID)
					}
				})
			}
		})
	}
}

func vlessCase(uuid string) protocolCase {
	return protocolCase{
		name:           "vless",
		network:        "tcp",
		serverTag:      "vless-in",
		clientProxyTag: "vless-out",
		secrets:        []string{uuid},
		serverInbound: func(listenPort int) map[string]any {
			return map[string]any{
				"type":        "vless",
				"tag":         "vless-in",
				"listen":      "127.0.0.1",
				"listen_port": listenPort,
				"users": []any{map[string]any{
					"name": "relaydeck-test",
					"uuid": uuid,
				}},
			}
		},
		clientOutbound: func(server string, serverPort int) map[string]any {
			return map[string]any{
				"type":        "vless",
				"tag":         "vless-out",
				"server":      server,
				"server_port": serverPort,
				"uuid":        uuid,
				"network":     "tcp",
			}
		},
	}
}

func vlessRealityCase(uuid, privateKey, publicKey, shortID string, decoyPort int) protocolCase {
	return protocolCase{
		name:           "vless-reality",
		network:        "tcp",
		serverTag:      "reality-in",
		clientProxyTag: "reality-out",
		secrets:        []string{uuid, privateKey, publicKey},
		serverInbound: func(listenPort int) map[string]any {
			return map[string]any{
				"type":        "vless",
				"tag":         "reality-in",
				"listen":      "127.0.0.1",
				"listen_port": listenPort,
				"users": []any{map[string]any{
					"name": "relaydeck-reality",
					"uuid": uuid,
					"flow": "xtls-rprx-vision",
				}},
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "localhost",
					"min_version": "1.3",
					"reality": map[string]any{
						"enabled":     true,
						"private_key": privateKey,
						"short_id":    []string{shortID},
						"handshake": map[string]any{
							"server":      "127.0.0.1",
							"server_port": decoyPort,
						},
					},
				},
			}
		},
		clientOutbound: func(server string, serverPort int) map[string]any {
			return map[string]any{
				"type":        "vless",
				"tag":         "reality-out",
				"server":      server,
				"server_port": serverPort,
				"uuid":        uuid,
				"flow":        "xtls-rprx-vision",
				"network":     "tcp",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "localhost",
					"utls": map[string]any{
						"enabled":     true,
						"fingerprint": "chrome",
					},
					"reality": map[string]any{
						"enabled":    true,
						"public_key": publicKey,
						"short_id":   shortID,
					},
				},
			}
		},
	}
}

func hysteria2Case(password, certificatePath, keyPath string) protocolCase {
	return protocolCase{
		name:           "hysteria2",
		network:        "udp",
		serverTag:      "hysteria2-in",
		clientProxyTag: "hysteria2-out",
		secrets:        []string{password},
		serverInbound: func(listenPort int) map[string]any {
			return map[string]any{
				"type":        "hysteria2",
				"tag":         "hysteria2-in",
				"listen":      "127.0.0.1",
				"listen_port": listenPort,
				"users": []any{map[string]any{
					"name":     "relaydeck-test",
					"password": password,
				}},
				"tls": map[string]any{
					"enabled":          true,
					"server_name":      "localhost",
					"alpn":             []string{"h3"},
					"min_version":      "1.3",
					"certificate_path": certificatePath,
					"key_path":         keyPath,
				},
			}
		},
		clientOutbound: func(server string, serverPort int) map[string]any {
			return map[string]any{
				"type":        "hysteria2",
				"tag":         "hysteria2-out",
				"server":      server,
				"server_port": serverPort,
				"password":    password,
				"network":     "tcp",
				"tls": map[string]any{
					"enabled":          true,
					"server_name":      "localhost",
					"alpn":             []string{"h3"},
					"min_version":      "1.3",
					"certificate_path": certificatePath,
				},
			}
		},
	}
}

func buildServerConfig(tag string, inbound map[string]any) map[string]any {
	return map[string]any{
		"log":       map[string]any{"level": "error", "timestamp": true},
		"inbounds":  []any{inbound},
		"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
		"route": map[string]any{
			"rules": []any{map[string]any{
				"inbound":  []string{tag},
				"action":   "route",
				"outbound": "direct",
			}},
			"final": "direct",
		},
	}
}

func buildClientConfig(proxyTag string, outbound map[string]any, directPort int, targetHost string, targetPort int) map[string]any {
	return map[string]any{
		"log": map[string]any{"level": "error", "timestamp": true},
		"inbounds": []any{map[string]any{
			"type":        "direct",
			"tag":         "direct-in",
			"listen":      "127.0.0.1",
			"listen_port": directPort,
			"network":     "tcp",
		}},
		"outbounds": []any{
			outbound,
			map[string]any{"type": "direct", "tag": "direct"},
		},
		"route": map[string]any{
			"rules": []any{map[string]any{
				"inbound":          []string{"direct-in"},
				"action":           "route",
				"outbound":         proxyTag,
				"override_address": targetHost,
				"override_port":    targetPort,
			}},
			"final": "direct",
		},
	}
}

func startForwardManager(t *testing.T, network string, serverPort, relay4Port, relay6Port int) *forward.Manager {
	t.Helper()
	target := net.JoinHostPort("127.0.0.1", fmt.Sprint(serverPort))
	rules := []config.Rule{
		{
			ID:             "relay-v4",
			Name:           "protocol relay IPv4",
			Enabled:        true,
			Network:        network,
			Listen:         net.JoinHostPort("127.0.0.1", fmt.Sprint(relay4Port)),
			Target:         target,
			MaxConnections: 64,
			UDPTimeout:     30,
		},
		{
			ID:             "relay-v6",
			Name:           "protocol relay IPv6",
			Enabled:        true,
			Network:        network,
			Listen:         net.JoinHostPort("::1", fmt.Sprint(relay6Port)),
			Target:         target,
			MaxConnections: 64,
			UDPTimeout:     30,
		},
	}
	manager := forward.NewManager()
	t.Cleanup(manager.Close)
	if err := manager.Apply(rules); err != nil {
		t.Fatalf("start RelayDeck manager: %v", err)
	}
	return manager
}

type singBoxProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	output  *boundedOutput
	secrets []string
	stop    sync.Once
}

func startSingBox(t *testing.T, binary string, value map[string]any, secrets []string) *singBoxProcess {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode sing-box config: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "sing-box.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatalf("write sing-box config: %v", err)
	}
	if output, err := runBoundedCommand(15*time.Second, binary, "check", "-c", configPath); err != nil {
		t.Fatalf("sing-box config check failed: %v: %s", err, redact(output, secrets))
	}

	output := &boundedOutput{limit: processOutputMax}
	cmd := exec.Command(binary, "run", "-c", configPath)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sing-box: %v", err)
	}
	process := &singBoxProcess{
		cmd:     cmd,
		done:    make(chan struct{}),
		output:  output,
		secrets: secrets,
	}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() { process.stopProcess() })

	select {
	case <-process.done:
		t.Fatalf("sing-box exited during startup: %v: %s", process.waitErr, process.outputText())
	case <-time.After(300 * time.Millisecond):
	}
	return process
}

func (p *singBoxProcess) stopProcess() {
	p.stop.Do(func() {
		select {
		case <-p.done:
			return
		default:
		}
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(2 * time.Second):
			}
			return
		}
		select {
		case <-p.done:
			return
		case <-time.After(4 * time.Second):
		}
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (p *singBoxProcess) requireRunning(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("sing-box exited: %v: %s", p.waitErr, p.outputText())
	default:
	}
}

func (p *singBoxProcess) outputText() string {
	return redact(p.output.String(), p.secrets)
}

type boundedOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedOutput) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			_, _ = b.buffer.Write(payload[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buffer.Write(payload)
		}
	} else {
		b.truncated = true
	}
	return len(payload), nil
}

func (b *boundedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := b.buffer.String()
	if b.truncated {
		text += "\n[output truncated]"
	}
	return text
}

func runBoundedCommand(timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	output := &boundedOutput{limit: 16 << 10}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return output.String(), errors.New("command timed out")
	}
	return output.String(), err
}

func waitTCP(t *testing.T, address string, process *singBoxProcess) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		process.requireRunning(t)
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sing-box server inbound did not start: %s", process.outputText())
}

func requestThroughDirect(
	t *testing.T,
	directAddress string,
	want string,
	clientProcess *singBoxProcess,
	serverProcess *singBoxProcess,
) {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}

	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		requestURL := "http://" + directAddress + "/echo?case=" + url.QueryEscape(strings.TrimPrefix(want, "relaydeck-e2e:"))
		response, err := client.Get(requestURL)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && string(body) == want {
				return
			}
			lastErr = fmt.Errorf("status=%d body=%q read=%v", response.StatusCode, body, readErr)
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf(
		"HTTP through sing-box failed: %v\nclient output:\n%s\nserver output:\n%s",
		lastErr,
		clientProcess.outputText(),
		serverProcess.outputText(),
	)
}

func startRealityDecoy(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "reality decoy")
	}))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2", "http/1.1"},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func createSelfSignedCertificate(dir string) (string, string, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return "", "", err
	}
	certPath := filepath.Join(dir, "hysteria2-cert.pem")
	keyPath := filepath.Join(dir, "hysteria2-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func generateRealityKeyPair() (string, string, error) {
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	private := base64.RawURLEncoding.EncodeToString(privateKey.Bytes())
	public := base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes())
	return private, public, nil
}

func randomHex(bytesCount int) (string, error) {
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4],
		value[4:6],
		value[6:8],
		value[8:10],
		value[10:16],
	), nil
}

func freePort(t *testing.T, network, host string) int {
	t.Helper()
	address := net.JoinHostPort(host, "0")
	switch network {
	case "tcp":
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("reserve TCP %s: %v", address, err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		return port
	case "udp":
		addr, err := net.ResolveUDPAddr("udp", address)
		if err != nil {
			t.Fatalf("resolve UDP %s: %v", address, err)
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			t.Fatalf("reserve UDP %s: %v", address, err)
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		_ = conn.Close()
		return port
	default:
		t.Fatalf("unsupported network %q", network)
		return 0
	}
}

func redact(text string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return text
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}
