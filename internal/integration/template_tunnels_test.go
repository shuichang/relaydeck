package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
	"github.com/shuichang/relaydeck/internal/engine"
	"github.com/shuichang/relaydeck/internal/forward"
)

// Exercise the actual catalog, including UDP inside a Reality/XUDP tunnel.
// The plaintext payload deliberately is not VLESS: tunnel selection must not
// impose an application protocol on the native relay's byte stream.
func TestTemplateTunnelsCarryTCPAndUDP(t *testing.T) {
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
	tcpTarget, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcpTarget.Close() })
	targetPort := tcpTarget.Addr().(*net.TCPAddr).Port
	udpTarget, err := net.ListenPacket("udp4", tcpTarget.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udpTarget.Close() })
	go func() {
		for {
			conn, err := tcpTarget.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
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
	tested := 0
	for _, template := range engine.Templates() {
		if template.ID != "direct-passthrough" && template.ID != "direct-vless-reality" {
			continue
		}
		tested++
		t.Run(template.ID, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(template.Config, &value); err != nil {
				t.Fatal(err)
			}
			inbound := value["inbounds"].([]any)[0].(map[string]any)
			if network, _ := inbound["network"].(string); network != "" {
				t.Fatalf("template limits application transport to %q", network)
			}
			localPort := freePort(t, "tcp", "127.0.0.1")
			inbound["listen_port"] = localPort
			rule := value["route"].(map[string]any)["rules"].([]any)[0].(map[string]any)
			rule["override_address"] = "127.0.0.1"
			rule["override_port"] = targetPort
			var secrets []string
			if template.ID == "direct-vless-reality" {
				decoy := startRealityDecoy(t)
				decoyPort := decoy.Listener.Addr().(*net.TCPAddr).Port
				privateKey, publicKey, err := generateRealityKeyPair()
				if err != nil {
					t.Fatal(err)
				}
				uuid, err := randomUUID()
				if err != nil {
					t.Fatal(err)
				}
				shortID, err := randomHex(4)
				if err != nil {
					t.Fatal(err)
				}
				protocol := vlessRealityCase(uuid, privateKey, publicKey, shortID, decoyPort)
				serverPort := freePort(t, "tcp", "127.0.0.1")
				server := startSingBox(t, binary, buildServerConfig(protocol.serverTag, protocol.serverInbound(serverPort)), protocol.secrets)
				waitTCP(t, fmt.Sprintf("127.0.0.1:%d", serverPort), server)
				outbound := value["outbounds"].([]any)[0].(map[string]any)
				outbound["server"] = "127.0.0.1"
				outbound["server_port"] = serverPort
				outbound["uuid"] = uuid
				tls := outbound["tls"].(map[string]any)
				tls["server_name"] = "localhost"
				reality := tls["reality"].(map[string]any)
				reality["public_key"], reality["short_id"] = publicKey, shortID
				secrets = protocol.secrets
			}
			client := startSingBox(t, binary, value, secrets)
			waitTCP(t, fmt.Sprintf("127.0.0.1:%d", localPort), client)
			manager := forward.NewManager()
			t.Cleanup(manager.Close)
			rules := make([]config.Rule, 0, 2)
			for i, host := range []string{"127.0.0.1", "::1"} {
				rules = append(rules, config.Rule{
					ID: fmt.Sprintf("tunnel-%d", i), Enabled: true, Network: "both",
					Listen: net.JoinHostPort(host, fmt.Sprint(freePort(t, "tcp", host))),
					Target: fmt.Sprintf("127.0.0.1:%d", localPort),
				})
			}
			if err := manager.Apply(rules); err != nil {
				t.Fatal(err)
			}
			for _, entry := range rules {
				for _, network := range []string{"tcp", "udp"} {
					t.Run(entry.ID+"/"+network, func(t *testing.T) {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						conn, err := (&net.Dialer{}).DialContext(ctx, network, entry.Listen)
						if err != nil {
							t.Fatal(err)
						}
						defer conn.Close()
						_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
						payload := []byte("plain application bytes inside " + template.ID + "/" + network)
						if _, err := conn.Write(payload); err != nil {
							t.Fatal(err)
						}
						reply := make([]byte, len(payload)+16)
						var n int
						if network == "tcp" {
							n, err = io.ReadFull(conn, reply[:len(payload)])
						} else {
							n, err = conn.Read(reply)
						}
						if err != nil || string(reply[:n]) != string(payload) {
							t.Fatalf("template did not carry %s application bytes: reply=%q error=%v\n%s", network, reply[:n], err, client.outputText())
						}
					})
				}
			}
		})
	}
	if tested != 2 {
		t.Fatalf("required direct and Reality templates not found: tested %d", tested)
	}
}
