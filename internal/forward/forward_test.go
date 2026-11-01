package forward

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

func TestValidateRulesRejectsInvalidAndConflictingRules(t *testing.T) {
	base := config.Rule{
		ID:             "base",
		Name:           "base",
		Enabled:        true,
		Network:        "tcp",
		Listen:         "127.0.0.1:12001",
		Target:         "localhost:9000",
		AllowedCIDRs:   []string{"127.0.0.0/8"},
		MaxConnections: 8,
		UDPTimeout:     10,
	}
	if err := ValidateRules([]config.Rule{base}); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}

	with := func(change func(*config.Rule)) config.Rule {
		rule := base
		rule.AllowedCIDRs = append([]string(nil), base.AllowedCIDRs...)
		change(&rule)
		return rule
	}
	cases := []struct {
		name  string
		rules []config.Rule
		want  string
	}{
		{"network", []config.Rule{with(func(r *config.Rule) { r.Network = "icmp" })}, "network"},
		{"listen hostname", []config.Rule{with(func(r *config.Rule) { r.Listen = "localhost:12001" })}, "listen host"},
		{"listen port", []config.Rule{with(func(r *config.Rule) { r.Listen = "127.0.0.1:0" })}, "port"},
		{"ipv6 target", []config.Rule{with(func(r *config.Rule) { r.Target = "[::1]:9000" })}, "IPv4"},
		{"empty target host", []config.Rule{with(func(r *config.Rule) { r.Target = ":9000" })}, "empty"},
		{"cidr", []config.Rule{with(func(r *config.Rule) { r.AllowedCIDRs = []string{"not-a-cidr"} })}, "CIDR"},
		{"long id", []config.Rule{with(func(r *config.Rule) { r.ID = strings.Repeat("i", maxRuleIDBytes+1) })}, "id exceeds"},
		{"long name", []config.Rule{with(func(r *config.Rule) { r.Name = strings.Repeat("n", maxRuleNameBytes+1) })}, "name exceeds"},
		{"too many cidrs", []config.Rule{with(func(r *config.Rule) {
			r.AllowedCIDRs = make([]string, maxAllowedCIDRs+1)
			for i := range r.AllowedCIDRs {
				r.AllowedCIDRs[i] = "127.0.0.0/8"
			}
		})}, "allowed_cidrs"},
		{"negative maximum", []config.Rule{with(func(r *config.Rule) { r.MaxConnections = -1 })}, "max_connections"},
		{"excessive timeout", []config.Rule{with(func(r *config.Rule) { r.UDPTimeout = 86401 })}, "udp_timeout"},
		{"duplicate id", []config.Rule{base, with(func(r *config.Rule) { r.Listen = "127.0.0.1:12002" })}, "duplicate"},
		{"conflicting listeners", []config.Rule{
			with(func(r *config.Rule) { r.Network = "both" }),
			with(func(r *config.Rule) {
				r.ID = "second"
				r.Network = "udp"
			}),
		}, "conflicts"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRules(test.rules)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateRules() error = %v, want substring %q", err, test.want)
			}
		})
	}

	ipv6Listen := with(func(r *config.Rule) {
		r.Listen = "[::]:12003"
		r.Target = "127.0.0.1:9000"
	})
	if err := ValidateRules([]config.Rule{ipv6Listen}); err != nil {
		t.Fatalf("IPv6 listener to IPv4 target rejected: %v", err)
	}

	disabledConflict := with(func(r *config.Rule) {
		r.ID = "disabled"
		r.Enabled = false
	})
	if err := ValidateRules([]config.Rule{base, disabledConflict}); err != nil {
		t.Fatalf("disabled listener conflict rejected: %v", err)
	}

	tooMany := make([]config.Rule, maxRuleCount+1)
	for i := range tooMany {
		tooMany[i] = config.Rule{
			ID:      fmt.Sprintf("rule-%d", i),
			Name:    "disabled",
			Enabled: false,
			Network: "tcp",
			Listen:  fmt.Sprintf("127.0.0.1:%d", 20000+i),
			Target:  "127.0.0.1:9",
		}
	}
	if err := ValidateRules(tooMany); err == nil || !strings.Contains(err.Error(), "too many rules") {
		t.Fatalf("maximum rule count error = %v", err)
	}
}

func TestTCPForwardingHalfCloseAndUnchangedRulePreservation(t *testing.T) {
	target, accepted := startTCPEcho(t, "127.0.0.1:0")
	_, listenPort, err := net.SplitHostPort(freeTCPAddress(t))
	if err != nil {
		t.Fatal(err)
	}
	listen := ":" + listenPort

	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "tcp-preserve",
		Name:           "before",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         target,
		AllowedCIDRs:   []string{"127.0.0.0/8"},
		MaxConnections: 8,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	client := dialTCP(t, listen)
	waitFor(t, 2*time.Second, "TCP session admission", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].ActiveConnections == 1 && accepted.Load() == 1
	})

	renamed := rule
	renamed.Name = "after"
	if err := manager.Apply([]config.Rule{renamed}); err != nil {
		t.Fatalf("Apply with unchanged runtime fields: %v", err)
	}
	snapshot := manager.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Name != "after" || !snapshot[0].Running {
		t.Fatalf("unexpected preserved snapshot: %+v", snapshot)
	}
	if snapshot[0].ActiveConnections != 1 {
		t.Fatalf("unchanged Apply closed the existing session: %+v", snapshot[0])
	}

	payload := []byte("request-through-existing-session")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write through preserved listener: %v", err)
	}
	waitFor(t, 2*time.Second, "real-time TCP byte counters", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 &&
			snapshot[0].BytesIn >= uint64(len(payload)) &&
			snapshot[0].BytesOut >= uint64(len(payload))
	})
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("half-close after request: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
	_ = client.Close()

	waitFor(t, 2*time.Second, "TCP session cleanup", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].ActiveConnections == 0 &&
			snapshot[0].BytesIn >= uint64(len(payload)) &&
			snapshot[0].BytesOut >= uint64(len(payload))
	})
}

func TestApplyRejectsDirectSelfForwarding(t *testing.T) {
	selfAddress := freeTCPAddress(t)
	_, port, err := net.SplitHostPort(selfAddress)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		network string
		listen  string
	}{
		{name: "TCP matching address", network: "tcp", listen: selfAddress},
		{name: "UDP matching address", network: "udp", listen: selfAddress},
		{name: "TCP wildcard", network: "tcp", listen: ":" + port},
		{name: "UDP IPv4 wildcard", network: "udp", listen: "0.0.0.0:" + port},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager()
			t.Cleanup(manager.Close)
			rule := config.Rule{
				ID:             "self-loop",
				Name:           "self-loop",
				Enabled:        true,
				Network:        test.network,
				Listen:         test.listen,
				Target:         selfAddress,
				MaxConnections: 4,
				UDPTimeout:     10,
			}
			if err := manager.Apply([]config.Rule{rule}); err == nil ||
				!strings.Contains(err.Error(), "own listener") {
				t.Fatalf("Apply self-forwarding error = %v", err)
			}
			if snapshot := manager.Snapshot(); len(snapshot) != 0 {
				t.Fatalf("rejected self-forwarding changed rules: %+v", snapshot)
			}
		})
	}
}

func TestTCPConnectionLimitAndChangedRuleClosure(t *testing.T) {
	target, _ := startTCPEcho(t, "127.0.0.1:0")
	listen := freeTCPAddress(t)

	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "tcp-limit",
		Name:           "limit",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 1,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	first := dialTCP(t, listen)
	defer first.Close()
	if _, err := first.Write([]byte("a")); err != nil {
		t.Fatalf("write first connection: %v", err)
	}
	one := make([]byte, 1)
	_ = first.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(first, one); err != nil || string(one) != "a" {
		t.Fatalf("first connection echo = %q, %v", one, err)
	}

	second := dialTCP(t, listen)
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, err := second.Read(one)
	if err == nil {
		t.Fatal("connection above max_connections remained open")
	}
	waitFor(t, time.Second, "limit error snapshot", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && strings.Contains(snapshot[0].Error, "connection limit")
	})

	changed := rule
	changed.MaxConnections = 2
	if err := manager.Apply([]config.Rule{changed}); err != nil {
		t.Fatalf("Apply changed limit: %v", err)
	}
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := first.Read(one); err == nil {
		t.Fatal("changed rule did not close its existing connection")
	}
	waitFor(t, time.Second, "replacement listener", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].Running && snapshot[0].ActiveConnections == 0
	})
}

func TestCloseDoesNotWaitForSilentUpstreamRead(t *testing.T) {
	target, accepted, releaseUpstream := startHoldingTCP(t)
	listen := freeTCPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "silent-upstream",
		Name:           "silent upstream",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 4,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	client := dialTCP(t, listen)
	defer client.Close()
	waitFor(t, 2*time.Second, "active proxy session", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].ActiveConnections == 1 && accepted.Load() == 1
	})

	closed := make(chan struct{})
	go func() {
		manager.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		releaseUpstream()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("Manager.Close blocked on a TCP read from a silent upstream")
	}

	snapshot := manager.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Running {
		t.Fatalf("unexpected closed snapshot: %+v", snapshot)
	}
}

func TestUDPForwardingZeroLengthTimeoutAndSessionLimit(t *testing.T) {
	target, accepted := startUDPEcho(t, "127.0.0.1:0")
	listen := freeUDPAddress(t)

	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "udp-sessions",
		Name:           "udp",
		Enabled:        true,
		Network:        "udp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 1,
		UDPTimeout:     30,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	first := dialUDP(t, listen)
	defer first.Close()
	sendUDPAndRead(t, first, []byte("hello"), []byte("hello"))
	zero := make([]byte, 1)
	if _, err := first.Write(nil); err != nil {
		t.Fatalf("write zero-length UDP datagram: %v", err)
	}
	_ = first.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := first.Read(zero)
	if err != nil {
		t.Fatalf("read zero-length UDP reply: %v", err)
	}
	if n != 0 {
		t.Fatalf("zero-length UDP reply length = %d", n)
	}
	waitFor(t, 2*time.Second, "UDP session creation", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].UDPSessions == 1 && accepted.Load() >= 2
	})

	second := dialUDP(t, listen)
	defer second.Close()
	if _, err := second.Write([]byte("blocked")); err != nil {
		t.Fatalf("write blocked UDP datagram: %v", err)
	}
	_ = second.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	_, err = second.Read(zero)
	var netErr net.Error
	if err == nil || !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("datagram above session limit returned %v, want read timeout", err)
	}
	waitFor(t, time.Second, "UDP limit error snapshot", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && strings.Contains(snapshot[0].Error, "udp session limit")
	})

	shortTimeout := rule
	shortTimeout.UDPTimeout = 1
	if err := manager.Apply([]config.Rule{shortTimeout}); err != nil {
		t.Fatalf("Apply short UDP timeout: %v", err)
	}
	sendUDPAndRead(t, first, []byte("recreated"), []byte("recreated"))
	waitFor(t, 4*time.Second, "idle UDP session reclamation", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].UDPSessions == 0
	})
	sendUDPAndRead(t, first, []byte("after-timeout"), []byte("after-timeout"))
	waitFor(t, time.Second, "UDP counters", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].BytesIn >= 12 && snapshot[0].BytesOut >= 12
	})
}

func TestConcurrentUDPClientsAndClose(t *testing.T) {
	target, _ := startUDPEcho(t, "127.0.0.1:0")
	listen := freeUDPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "udp-concurrent",
		Name:           "concurrent UDP",
		Enabled:        true,
		Network:        "udp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 16,
		UDPTimeout:     30,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	const clients = 12
	connections := make([]*net.UDPConn, clients)
	for i := range connections {
		connections[i] = dialUDP(t, listen)
		defer connections[i].Close()
	}

	errorsCh := make(chan error, clients)
	var wait sync.WaitGroup
	for i, conn := range connections {
		wait.Add(1)
		go func() {
			defer wait.Done()
			payload := []byte(fmt.Sprintf("client-%02d", i))
			if _, err := conn.Write(payload); err != nil {
				errorsCh <- fmt.Errorf("client %d write: %w", i, err)
				return
			}
			buffer := make([]byte, len(payload))
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := conn.Read(buffer)
			if err != nil {
				errorsCh <- fmt.Errorf("client %d read: %w", i, err)
				return
			}
			if string(buffer[:n]) != string(payload) {
				errorsCh <- fmt.Errorf("client %d echo = %q, want %q", i, buffer[:n], payload)
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	waitFor(t, 2*time.Second, "concurrent UDP sessions", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].UDPSessions == clients
	})

	manager.Close()
	manager.Close()
	snapshot := manager.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Running {
		t.Fatalf("closed manager snapshot = %+v", snapshot)
	}
	_ = connections[0].SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := connections[0].Write([]byte("after-close")); err != nil {
		t.Fatalf("write after Close: %v", err)
	}
	if _, err := connections[0].Read(make([]byte, 16)); err == nil {
		t.Fatal("UDP listener still answered after Close")
	}
}

func TestBothNetworkUsesIPv4ResolvedHostname(t *testing.T) {
	udpTarget, _ := startUDPEcho(t, "127.0.0.1:0")
	_, udpPort, err := net.SplitHostPort(udpTarget)
	if err != nil {
		t.Fatal(err)
	}
	tcpTarget, _ := startTCPEcho(t, net.JoinHostPort("127.0.0.1", udpPort))
	if tcpTarget != udpTarget {
		t.Fatalf("shared target mismatch: tcp=%s udp=%s", tcpTarget, udpTarget)
	}

	listen := freeTCPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "both",
		Name:           "both",
		Enabled:        true,
		Network:        "both",
		Listen:         listen,
		Target:         "localhost:" + udpPort,
		MaxConnections: 8,
		UDPTimeout:     5,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply both: %v", err)
	}

	client := dialTCP(t, listen)
	payload := []byte("both")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("TCP write: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("TCP half-close: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(client)
	_ = client.Close()
	if err != nil || string(got) != string(payload) {
		t.Fatalf("TCP side of both rule = %q, %v", got, err)
	}

	udpClient := dialUDP(t, listen)
	defer udpClient.Close()
	sendUDPAndRead(t, udpClient, payload, payload)
	snapshot := manager.Snapshot()
	if len(snapshot) != 1 || !snapshot[0].Running || snapshot[0].UDPSessions == 0 {
		t.Fatalf("unexpected both snapshot: %+v", snapshot)
	}
}

func TestIPv6ListenerForwardsToIPv4Target(t *testing.T) {
	listener, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.ParseIP("::1"), Port: 0})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	listen := listener.Addr().String()
	_ = listener.Close()

	target, _ := startTCPEcho(t, "127.0.0.1:0")
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "v6-input",
		Name:           "IPv6 input",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 4,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply IPv6 listener: %v", err)
	}

	conn, err := net.DialTimeout("tcp6", listen, 2*time.Second)
	if err != nil {
		t.Fatalf("dial IPv6 listener: %v", err)
	}
	payload := []byte("v6-to-v4")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("IPv6 client write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("IPv6 client half-close: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil || string(got) != string(payload) {
		t.Fatalf("IPv6-to-IPv4 echo = %q, %v", got, err)
	}
}

func TestCIDRWhitelistAllowsAndRejectsLoopback(t *testing.T) {
	target, _ := startTCPEcho(t, "127.0.0.1:0")
	listen := freeTCPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "cidr",
		Name:           "CIDR",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         target,
		AllowedCIDRs:   []string{"192.0.2.0/24"},
		MaxConnections: 4,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply denied CIDR: %v", err)
	}

	denied := dialTCP(t, listen)
	_ = denied.SetReadDeadline(time.Now().Add(time.Second))
	one := make([]byte, 1)
	if _, err := denied.Read(one); err == nil {
		t.Fatal("connection outside allowed_cidrs remained open")
	}
	_ = denied.Close()
	waitFor(t, time.Second, "CIDR rejection snapshot", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && strings.Contains(snapshot[0].Error, "not allowed")
	})

	allowed := rule
	allowed.AllowedCIDRs = []string{"127.0.0.0/8"}
	if err := manager.Apply([]config.Rule{allowed}); err != nil {
		t.Fatalf("Apply allowed CIDR: %v", err)
	}
	conn := dialTCP(t, listen)
	defer conn.Close()
	if _, err := conn.Write([]byte("ok")); err != nil {
		t.Fatalf("allowed write: %v", err)
	}
	_ = conn.CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "ok" {
		t.Fatalf("allowed echo = %q, %v", got, err)
	}
}

func TestApplyRollbackRestoresStoppedListenerAndCounters(t *testing.T) {
	target, _ := startTCPEcho(t, "127.0.0.1:0")
	listen := freeTCPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	original := config.Rule{
		ID:             "rollback",
		Name:           "original",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 4,
	}
	if err := manager.Apply([]config.Rule{original}); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}
	before := dialTCP(t, listen)
	if _, err := before.Write([]byte("warm")); err != nil {
		t.Fatalf("warm-up write: %v", err)
	}
	warm := make([]byte, 4)
	_ = before.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(before, warm); err != nil {
		t.Fatalf("warm-up read: %v", err)
	}
	_ = before.Close()
	waitFor(t, 2*time.Second, "warm-up counters", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].BytesIn >= 4 && snapshot[0].BytesOut >= 4
	})
	oldBytesIn := manager.Snapshot()[0].BytesIn
	oldBytesOut := manager.Snapshot()[0].BytesOut

	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	bad := config.Rule{
		ID:             "cannot-bind",
		Name:           "occupied",
		Enabled:        true,
		Network:        "tcp",
		Listen:         occupied.Addr().String(),
		Target:         target,
		MaxConnections: 1,
	}
	replacement := original
	replacement.MaxConnections = 5
	if err := manager.Apply([]config.Rule{replacement, bad}); err == nil {
		t.Fatal("Apply unexpectedly succeeded with an occupied listener")
	}

	snapshot := manager.Snapshot()
	if len(snapshot) != 1 || snapshot[0].ID != original.ID || snapshot[0].Name != original.Name ||
		snapshot[0].MaxConnections != original.MaxConnections || !snapshot[0].Running {
		t.Fatalf("rule set did not roll back: %+v", snapshot)
	}
	if snapshot[0].BytesIn < oldBytesIn || snapshot[0].BytesOut < oldBytesOut {
		t.Fatalf("rollback lost counters: before=%d/%d after=%d/%d",
			oldBytesIn, oldBytesOut, snapshot[0].BytesIn, snapshot[0].BytesOut)
	}

	after := dialTCP(t, listen)
	defer after.Close()
	if _, err := after.Write([]byte("again")); err != nil {
		t.Fatalf("write through rolled-back listener: %v", err)
	}
	_ = after.CloseWrite()
	_ = after.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(after)
	if err != nil || string(got) != "again" {
		t.Fatalf("rolled-back listener echo = %q, %v", got, err)
	}
}

func startTCPEcho(t *testing.T, address string) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("start TCP echo listener: %v", err)
	}
	var accepted atomic.Int64
	var connWG sync.WaitGroup
	var connsMu sync.Mutex
	conns := make(map[net.Conn]struct{})
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			connsMu.Lock()
			conns[conn] = struct{}{}
			connsMu.Unlock()
			connWG.Add(1)
			go func() {
				defer connWG.Done()
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
				connsMu.Lock()
				delete(conns, conn)
				connsMu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		connsMu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		connsMu.Unlock()
		connWG.Wait()
	})
	return listener.Addr().String(), &accepted
}

func startHoldingTCP(t *testing.T) (string, *atomic.Int64, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start holding TCP listener: %v", err)
	}
	var accepted atomic.Int64
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseCh) })
	}
	var connWG sync.WaitGroup
	var connsMu sync.Mutex
	conns := make(map[net.Conn]struct{})
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			connsMu.Lock()
			conns[conn] = struct{}{}
			connsMu.Unlock()
			connWG.Add(1)
			go func() {
				defer connWG.Done()
				<-releaseCh
				_ = conn.Close()
				connsMu.Lock()
				delete(conns, conn)
				connsMu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		release()
		connWG.Wait()
		connsMu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		connsMu.Unlock()
	})
	return listener.Addr().String(), &accepted, release
}

func startUDPEcho(t *testing.T, address string) (string, *atomic.Int64) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", mustResolveUDP(t, address))
	if err != nil {
		t.Fatalf("start UDP echo socket: %v", err)
	}
	var accepted atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 65535)
		for {
			n, remote, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			accepted.Add(1)
			if _, err := conn.WriteToUDP(buffer[:n], remote); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	return conn.LocalAddr().String(), &accepted
}

func mustResolveUDP(t *testing.T, address string) *net.UDPAddr {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		t.Fatalf("resolve UDP address %q: %v", address, err)
	}
	return addr
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func freeUDPAddress(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("reserve UDP address: %v", err)
	}
	address := conn.LocalAddr().String()
	_ = conn.Close()
	return address
}

func dialTCP(t *testing.T, address string) *net.TCPConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", address, 2*time.Second)
	if err != nil {
		t.Fatalf("dial TCP relay %s: %v", address, err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	return conn.(*net.TCPConn)
}

func dialUDP(t *testing.T, address string) *net.UDPConn {
	t.Helper()
	remote := mustResolveUDP(t, address)
	local := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	conn, err := net.DialUDP("udp4", local, remote)
	if err != nil {
		t.Fatalf("dial UDP relay %s: %v", address, err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	return conn
}

func sendUDPAndRead(t *testing.T, conn *net.UDPConn, payload, want []byte) {
	t.Helper()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write UDP payload: %v", err)
	}
	buffer := make([]byte, len(want)+1)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("read UDP payload: %v", err)
	}
	if string(buffer[:n]) != string(want) {
		t.Fatalf("UDP echo = %q, want %q", buffer[:n], want)
	}
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
