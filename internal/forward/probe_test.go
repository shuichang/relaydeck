package forward

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

func probeTestRule(t *testing.T) (config.Rule, net.Listener) {
	t.Helper()
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := port.Addr().String()
	_ = port.Close()
	return config.Rule{
		ID: "probe", Name: "draft", Network: "tcp",
		Listen: address, Target: target.Addr().String(),
	}, target
}

func TestProbeDraftDoesNotApplyOrKeepPorts(t *testing.T) {
	rule, _ := probeTestRule(t)
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := m.Probe(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	if result.Listen.Status != "available" || result.Listen.LatencyMS != nil {
		t.Fatalf("unstarted draft reported listening or fake RTT: %+v", result.Listen)
	}
	if result.Target.Status != "reachable" || result.Target.LatencyMS == nil || result.TargetAddress != rule.Target {
		t.Fatalf("reachable target not measured: %+v", result)
	}
	if len(m.Snapshot()) != 0 {
		t.Fatal("probe changed active rules")
	}
	listener, err := net.Listen("tcp4", rule.Listen)
	if err != nil {
		t.Fatalf("probe retained draft port: %v", err)
	}
	_ = listener.Close()
}

func TestProbeSavedListenerAndOtherRuleConflict(t *testing.T) {
	rule, _ := probeTestRule(t)
	rule.Enabled = true
	m := NewManager()
	defer m.Close()
	if err := m.Apply([]config.Rule{rule}); err != nil {
		t.Fatal(err)
	}
	result, err := m.Probe(context.Background(), rule)
	if err != nil || result.Listen.Status != "listening" || result.Listen.LatencyMS != nil {
		t.Fatalf("saved listener not detected: %+v %v", result, err)
	}
	rule.ID = ""
	result, err = m.Probe(context.Background(), rule)
	if err != nil || result.Listen.Status != "error" {
		t.Fatalf("another rule's occupied port accepted for a new draft: %+v %v", result, err)
	}
	if rows := m.Snapshot(); len(rows) != 1 || !rows[0].Running {
		t.Fatal("probe disrupted existing rule")
	}
}

func TestProbeSavedListenerDoesNotUseForwardingSlots(t *testing.T) {
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	defer func() {
		_ = target.Close()
		<-done
		close(accepted)
		for conn := range accepted {
			_ = conn.Close()
		}
	}()
	rule := config.Rule{
		ID: "one-slot", Enabled: true, Network: "tcp",
		Listen: freeTCPAddress(t), Target: target.Addr().String(), MaxConnections: 1,
	}
	m := NewManager()
	defer m.Close()
	if err := m.Apply([]config.Rule{rule}); err != nil {
		t.Fatal(err)
	}
	result, err := m.Probe(context.Background(), rule)
	if err != nil || result.Listen.Status != "listening" || result.Target.Status != "reachable" {
		t.Fatalf("probe failed: %+v %v", result, err)
	}
	time.Sleep(20 * time.Millisecond)
	if rows := m.Snapshot(); rows[0].ActiveConnections != 0 {
		t.Fatalf("probe spent a forwarding slot against a silent target: %+v", rows)
	}
}

func TestProbeBindWaitsForApplyAndCanBeCanceled(t *testing.T) {
	rule, _ := probeTestRule(t)
	m := NewManager()
	defer m.Close()
	m.applyMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	done := make(chan ProbeResult, 1)
	go func() {
		result, _ := m.Probe(ctx, rule)
		done <- result
	}()
	// While an Apply transaction owns the lifecycle lock, a probe must not
	// acquire its draft port even if the transaction has temporarily freed it.
	listener, err := net.Listen("tcp4", rule.Listen)
	if err != nil {
		m.applyMu.Unlock()
		t.Fatal(err)
	}
	_ = listener.Close()
	select {
	case result := <-done:
		m.applyMu.Unlock()
		if result.Listen.Status != "error" || !strings.Contains(result.Listen.Message, "超时") {
			t.Fatalf("probe did not honor cancelable Apply coordination: %+v", result)
		}
	case <-time.After(time.Second):
		m.applyMu.Unlock()
		t.Fatal("probe remained blocked behind Apply after its deadline")
	}
	rule.Enabled = true
	if err := m.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("probe retained a port after cancellation: %v", err)
	}
}

func TestProbeUDPDoesNotInventReachabilityOrSendPackets(t *testing.T) {
	rule, _ := probeTestRule(t)
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	rule.Network, rule.Target = "udp", target.LocalAddr().String()
	m := NewManager()
	defer m.Close()
	result, err := m.Probe(context.Background(), rule)
	if err != nil {
		t.Fatal(err)
	}
	if result.Target.Status != "unverified" || result.Target.LatencyMS != nil || result.Listen.LatencyMS != nil {
		t.Fatalf("UDP socket was mistaken for a round trip: %+v", result)
	}
	_ = target.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := target.ReadFromUDP(make([]byte, 32)); err == nil {
		t.Fatal("UDP probe injected unsolicited protocol data")
	}
}

func TestProbeBothMakesUDPUnverifiedExplicit(t *testing.T) {
	rule, _ := probeTestRule(t)
	rule.Network = "both"
	m := NewManager()
	defer m.Close()
	result, err := m.Probe(context.Background(), rule)
	if err != nil || result.Target.Status != "reachable" || result.Target.UDPStatus != "unverified" {
		t.Fatalf("mixed transport result lost UDP caveat: %+v %v", result, err)
	}
	if !strings.Contains(result.Target.Message, "UDP 尚未验证") {
		t.Fatal("TCP handshake misleadingly presented as UDP reachability")
	}
}

func TestProbeRefusedTargetAndIndependentIngress(t *testing.T) {
	rule, target := probeTestRule(t)
	_ = target.Close()
	m := NewManager()
	defer m.Close()
	result, err := m.Probe(context.Background(), rule)
	if err != nil || result.Target.Status != "error" || result.Target.LatencyMS != nil || result.Listen.Status != "available" {
		t.Fatalf("independent ingress/target results incorrect: %+v %v", result, err)
	}
}

func TestProbeValidationCancellationAndLoop(t *testing.T) {
	rule, _ := probeTestRule(t)
	m := NewManager()
	defer m.Close()
	invalid := rule
	invalid.Target = "[::1]:443"
	if _, err := m.Probe(context.Background(), invalid); err == nil {
		t.Fatal("non-IPv4 target was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	result, err := m.Probe(ctx, rule)
	if err != nil || result.Listen.Status != "error" || result.Target.Status != "error" || time.Since(start) > time.Second {
		t.Fatalf("cancellation was ignored: %+v %v", result, err)
	}
	rule.Target = rule.Listen
	result, err = m.Probe(context.Background(), rule)
	if err != nil || result.Target.Status != "error" || !strings.Contains(result.Target.Message, "环路") {
		t.Fatalf("self forwarding was not diagnosed: %+v %v", result, err)
	}
}
