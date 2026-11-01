//go:build linux

package forward

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

func TestUDPWildcardReplySourceAndSessionIsolation(t *testing.T) {
	target, _ := startUDPEcho(t, "127.0.0.1:0")
	relayPort := freeUDPPortOn(t, "udp4", "0.0.0.0")
	relayAddress := fmt.Sprintf("0.0.0.0:%d", relayPort)
	manager := startUDPTestManager(t, relayAddress, target)

	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("open unconnected UDP client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))

	destinations := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 2)}
	for index, destination := range destinations {
		payload := []byte(fmt.Sprintf("destination-%d", index))
		if _, err := client.WriteToUDP(payload, &net.UDPAddr{IP: destination, Port: relayPort}); err != nil {
			t.Fatalf("send to %s: %v", destination, err)
		}
		reply := make([]byte, 64)
		n, source, err := client.ReadFromUDP(reply)
		if err != nil {
			t.Fatalf("receive reply from %s: %v", destination, err)
		}
		if string(reply[:n]) != string(payload) {
			t.Fatalf("reply through %s = %q, want %q", destination, reply[:n], payload)
		}
		if !source.IP.Equal(destination) || source.Port != relayPort {
			t.Fatalf("reply source = %s, want %s:%d", source, destination, relayPort)
		}
	}

	waitFor(t, 2*time.Second, "two local-destination UDP sessions", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].UDPSessions == 2
	})
	sessions := snapshotUDPSessions(t, manager, "udp-wildcard")
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	locals := make(map[netip.Addr]bool)
	var clientAddress netip.AddrPort
	for _, session := range sessions {
		if !session.local.IsValid() {
			t.Fatalf("session did not retain its local destination: %+v", session.key)
		}
		locals[session.local.Addr().Unmap()] = true
		if clientAddress.IsValid() && clientAddress != session.client {
			t.Fatalf("same UDP client was split across remote keys: %s and %s", clientAddress, session.client)
		}
		clientAddress = session.client
	}
	if !locals[netip.MustParseAddr("127.0.0.1")] || !locals[netip.MustParseAddr("127.0.0.2")] {
		t.Fatalf("session local destinations = %v", locals)
	}

	// A connected socket rejects replies from a different local source. This
	// is the same failure mode seen by clients that call connect(2).
	for _, destination := range destinations {
		conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: destination, Port: relayPort})
		if err != nil {
			t.Fatalf("dial connected UDP %s: %v", destination, err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		payload := []byte("connected-" + destination.String())
		if _, err := conn.Write(payload); err != nil {
			_ = conn.Close()
			t.Fatalf("write connected UDP %s: %v", destination, err)
		}
		reply := make([]byte, 64)
		n, err := conn.Read(reply)
		_ = conn.Close()
		if err != nil {
			t.Fatalf("connected UDP did not receive a reply from %s: %v", destination, err)
		}
		if string(reply[:n]) != string(payload) {
			t.Fatalf("connected UDP reply = %q, want %q", reply[:n], payload)
		}
	}
}

func TestUDPIPv6WildcardAndSpecificReplySource(t *testing.T) {
	target, _ := startUDPEcho(t, "127.0.0.1:0")

	t.Run("dual-stack wildcard", func(t *testing.T) {
		relayPort := freeUDPPortOn(t, "udp6", "::")
		manager := startUDPTestManager(t, fmt.Sprintf("[::]:%d", relayPort), target)

		exerciseUDPExchange(t, "udp6", "::1", &net.UDPAddr{IP: net.ParseIP("::1"), Port: relayPort}, net.ParseIP("::1"))
		exerciseUDPExchange(t, "udp4", "127.0.0.1", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relayPort}, net.IPv4(127, 0, 0, 1))
		exerciseConnectedUDPExchange(t, "udp6", "::1", &net.UDPAddr{IP: net.ParseIP("::1"), Port: relayPort})
		exerciseConnectedUDPExchange(t, "udp4", "127.0.0.1", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relayPort})

		waitFor(t, 2*time.Second, "dual-stack UDP sessions", func() bool {
			snapshot := manager.Snapshot()
			return len(snapshot) == 1 && snapshot[0].UDPSessions == 4
		})
	})

	t.Run("specific IPv6", func(t *testing.T) {
		relayPort := freeUDPPortOn(t, "udp6", "::1")
		manager := startUDPTestManager(t, fmt.Sprintf("[::1]:%d", relayPort), target)
		exerciseUDPExchange(t, "udp6", "::1", &net.UDPAddr{IP: net.ParseIP("::1"), Port: relayPort}, net.ParseIP("::1"))
		exerciseConnectedUDPExchange(t, "udp6", "::1", &net.UDPAddr{IP: net.ParseIP("::1"), Port: relayPort})

		sessions := snapshotUDPSessions(t, manager, "udp-wildcard")
		if len(sessions) != 2 {
			t.Fatalf("specific IPv6 sessions = %+v", sessions)
		}
		for _, session := range sessions {
			if !session.local.Addr().Is6() {
				t.Fatalf("specific IPv6 session lost its local source: %+v", session.key)
			}
		}
	})
}

func TestPacketInfoControlMessageLayout(t *testing.T) {
	required := syscall.CmsgSpace(syscall.SizeofInet4Pktinfo) +
		syscall.CmsgSpace(syscall.SizeofInet6Pktinfo)
	if udpControlBufferSize() < required {
		t.Fatalf("UDP control buffer = %d, need at least %d for both cmsg families", udpControlBufferSize(), required)
	}

	ipv4 := netip.MustParseAddr("127.0.0.2")
	oob, err := buildPacketInfoOOB(udpPacketInfoIPv4, ipv4, 7)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := syscall.ParseSocketControlMessage(oob)
	if err != nil || len(messages) != 1 {
		t.Fatalf("parse IPv4 control message: messages=%d err=%v", len(messages), err)
	}
	if messages[0].Header.Level != syscall.IPPROTO_IP ||
		messages[0].Header.Type != syscall.IP_PKTINFO ||
		binary.NativeEndian.Uint32(messages[0].Data[0:4]) != 7 {
		t.Fatalf("unexpected IPv4 control message: %+v", messages[0])
	}
	if !net.IP(messages[0].Data[4:8]).Equal(net.IP(ipv4.AsSlice())) {
		t.Fatalf("IPv4 packet source = %v", net.IP(messages[0].Data[4:8]))
	}

	ipv6 := netip.MustParseAddr("2001:db8::5")
	oob, err = buildPacketInfoOOB(udpPacketInfoIPv6, ipv6, 9)
	if err != nil {
		t.Fatal(err)
	}
	messages, err = syscall.ParseSocketControlMessage(oob)
	if err != nil || len(messages) != 1 {
		t.Fatalf("parse IPv6 control message: messages=%d err=%v", len(messages), err)
	}
	if messages[0].Header.Level != syscall.IPPROTO_IPV6 ||
		messages[0].Header.Type != syscall.IPV6_PKTINFO ||
		binary.NativeEndian.Uint32(messages[0].Data[16:20]) != 9 {
		t.Fatalf("unexpected IPv6 control message: %+v", messages[0])
	}
	if !net.IP(messages[0].Data[:16]).Equal(ipv6.AsSlice()) {
		t.Fatalf("IPv6 packet source = %v", net.IP(messages[0].Data[:16]))
	}
}

func startUDPTestManager(t *testing.T, listen, target string) *Manager {
	t.Helper()
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "udp-wildcard",
		Name:           "UDP local source",
		Enabled:        true,
		Network:        "udp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 16,
		UDPTimeout:     30,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply UDP rule: %v", err)
	}
	return manager
}

func exerciseUDPExchange(t *testing.T, network, localHost string, relay *net.UDPAddr, wantSource net.IP) {
	t.Helper()
	localIP := net.ParseIP(localHost)
	if localIP == nil {
		t.Fatalf("invalid local IP %q", localHost)
	}
	client, err := net.ListenUDP(network, &net.UDPAddr{IP: localIP})
	if err != nil {
		t.Fatalf("listen %s: %v", network, err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))

	payload := []byte("udp-source-" + network)
	if _, err := client.WriteToUDP(payload, relay); err != nil {
		t.Fatalf("send UDP to %s: %v", relay, err)
	}
	reply := make([]byte, 64)
	n, source, err := client.ReadFromUDP(reply)
	if err != nil {
		t.Fatalf("receive UDP from %s: %v", relay, err)
	}
	if string(reply[:n]) != string(payload) {
		t.Fatalf("UDP reply = %q, want %q", reply[:n], payload)
	}
	if !source.IP.Equal(wantSource) || source.Port != relay.Port {
		t.Fatalf("UDP reply source = %s, want %s:%d", source, wantSource, relay.Port)
	}
}

func exerciseConnectedUDPExchange(t *testing.T, network, localHost string, relay *net.UDPAddr) {
	t.Helper()
	localAddr := &net.UDPAddr{IP: net.ParseIP(localHost)}
	conn, err := net.DialUDP(network, localAddr, relay)
	if err != nil {
		t.Fatalf("dial connected %s UDP: %v", network, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	payload := []byte("connected-" + network + "-" + localHost)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write connected UDP: %v", err)
	}
	reply := make([]byte, 64)
	n, err := conn.Read(reply)
	if err != nil {
		t.Fatalf("connected %s UDP did not accept the reply source: %v", network, err)
	}
	if string(reply[:n]) != string(payload) {
		t.Fatalf("connected UDP reply = %q, want %q", reply[:n], payload)
	}
}

func snapshotUDPSessions(t *testing.T, manager *Manager, ruleID string) []*udpSession {
	t.Helper()
	manager.mu.RLock()
	rt := manager.runtime[ruleKey("id:"+ruleID)]
	manager.mu.RUnlock()
	if rt == nil || rt.udp == nil {
		t.Fatalf("missing UDP runtime %q", ruleID)
	}
	rt.udp.mu.Lock()
	defer rt.udp.mu.Unlock()
	result := make([]*udpSession, 0, len(rt.udp.sessions))
	for _, session := range rt.udp.sessions {
		result = append(result, session)
	}
	return result
}

func freeUDPPortOn(t *testing.T, network, host string) int {
	t.Helper()
	address := net.JoinHostPort(host, "0")
	resolved, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		t.Fatalf("resolve UDP %s: %v", address, err)
	}
	conn, err := net.ListenUDP(network, resolved)
	if err != nil {
		t.Fatalf("reserve UDP %s: %v", address, err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}
