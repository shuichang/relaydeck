package forward

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

func TestTCPFailureClearsAfterUpstreamRecovery(t *testing.T) {
	targetAddress := freeTCPAddress(t)
	listen := freeTCPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)

	rule := config.Rule{
		ID:             "tcp-error-recovery",
		Name:           "tcp error recovery",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         targetAddress,
		MaxConnections: 4,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	failedClient := dialTCP(t, listen)
	_ = failedClient.Close()
	waitFor(t, 2*time.Second, "failed TCP target", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && strings.Contains(snapshot[0].Error, "dial target")
	})
	failed := manager.Snapshot()[0]
	if failed.LastError != failed.Error || failed.LastErrorNetwork != "tcp" {
		t.Fatalf("failed TCP snapshot did not expose its history: %+v", failed)
	}
	errorAt := parseSnapshotTime(t, failed.LastErrorAt)
	if failed.LastRecoveryAt != "" {
		t.Fatalf("failed TCP unexpectedly reported a recovery: %+v", failed)
	}

	target, _ := startTCPEcho(t, targetAddress)
	client := dialTCP(t, listen)
	payload := []byte("recovered")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write after target recovery: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("half-close after target recovery: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(client)
	_ = client.Close()
	if err != nil || string(got) != string(payload) {
		t.Fatalf("recovered echo = %q, %v; target=%s", got, err, target)
	}

	waitFor(t, 2*time.Second, "TCP error recovery", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].Error == "" && snapshot[0].LastRecoveryAt != ""
	})
	recovered := manager.Snapshot()[0]
	if recovered.LastError != failed.LastError ||
		recovered.LastErrorAt != failed.LastErrorAt ||
		recovered.LastErrorNetwork != "tcp" {
		t.Fatalf("TCP recovery lost error history: before=%+v after=%+v", failed, recovered)
	}
	recoveryAt := parseSnapshotTime(t, recovered.LastRecoveryAt)
	if recoveryAt.Before(errorAt) {
		t.Fatalf("recovery time %s precedes error time %s", recoveryAt, errorAt)
	}
}

func TestUDPErrorRequiresTargetReplyToRecover(t *testing.T) {
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	listen := freeUDPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "udp-reply-recovery",
		Name:           "udp reply recovery",
		Enabled:        true,
		Network:        "udp",
		Listen:         listen,
		Target:         target.LocalAddr().String(),
		MaxConnections: 4,
		UDPTimeout:     30,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rt := runtimeForRule(t, manager, rule.ID)
	rt.setUDPError(errors.New("udp target rejected an earlier packet"))

	client := dialUDP(t, listen)
	defer client.Close()
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatalf("send UDP request: %v", err)
	}

	buffer := make([]byte, 64)
	_ = target.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, remote, err := target.ReadFromUDP(buffer)
	if err != nil || string(buffer[:n]) != "request" {
		t.Fatalf("silent target receive = %q, %v", buffer[:n], err)
	}
	beforeReply := manager.Snapshot()[0]
	if !strings.Contains(beforeReply.Error, "udp target rejected") ||
		beforeReply.LastRecoveryAt != "" {
		t.Fatalf("successful UDP dial/write incorrectly cleared the error: %+v", beforeReply)
	}

	if _, err := target.WriteToUDP([]byte("reply"), remote); err != nil {
		t.Fatalf("send UDP reply: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = client.Read(buffer)
	if err != nil || string(buffer[:n]) != "reply" {
		t.Fatalf("UDP reply = %q, %v", buffer[:n], err)
	}
	waitFor(t, 2*time.Second, "UDP recovery after target reply", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].Error == "" && snapshot[0].LastRecoveryAt != ""
	})
	afterReply := manager.Snapshot()[0]
	if afterReply.LastError != "udp target rejected an earlier packet" ||
		afterReply.LastErrorNetwork != "udp" {
		t.Fatalf("UDP recovery lost history: %+v", afterReply)
	}
	parseSnapshotTime(t, afterReply.LastErrorAt)
	parseSnapshotTime(t, afterReply.LastRecoveryAt)
}

func TestBothUDPRejectionSurvivesTCPSuccess(t *testing.T) {
	udpTarget, _ := startUDPEcho(t, "127.0.0.1:0")
	_, port, err := net.SplitHostPort(udpTarget)
	if err != nil {
		t.Fatal(err)
	}
	startTCPEcho(t, net.JoinHostPort("127.0.0.1", port))

	listen := freeTCPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "both-error-isolation",
		Name:           "both error isolation",
		Enabled:        true,
		Network:        "both",
		Listen:         listen,
		Target:         net.JoinHostPort("127.0.0.1", port),
		MaxConnections: 1,
		UDPTimeout:     30,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	udpClient := dialUDP(t, listen)
	defer udpClient.Close()
	sendUDPAndRead(t, udpClient, []byte("hold-session"), []byte("hold-session"))

	rejectedUDPClient := dialUDP(t, listen)
	defer rejectedUDPClient.Close()
	if _, err := rejectedUDPClient.Write([]byte("rejected")); err != nil {
		t.Fatalf("write rejected UDP client: %v", err)
	}
	waitFor(t, 2*time.Second, "UDP rejection error", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && strings.Contains(snapshot[0].Error, "udp session limit")
	})

	tcpClient := dialTCP(t, listen)
	payload := []byte("tcp-still-works")
	if _, err := tcpClient.Write(payload); err != nil {
		t.Fatalf("TCP write beside UDP rejection: %v", err)
	}
	if err := tcpClient.CloseWrite(); err != nil {
		t.Fatalf("TCP half-close beside UDP rejection: %v", err)
	}
	_ = tcpClient.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(tcpClient)
	_ = tcpClient.Close()
	if err != nil || string(got) != string(payload) {
		t.Fatalf("TCP echo beside UDP rejection = %q, %v", got, err)
	}

	snapshot := manager.Snapshot()[0]
	if !strings.Contains(snapshot.Error, "udp session limit") ||
		snapshot.ErrorNetwork != "udp" ||
		snapshot.LastErrorNetwork != "udp" {
		t.Fatalf("TCP success cleared or relabeled the UDP rejection: %+v", snapshot)
	}

	sendUDPAndRead(t, udpClient, []byte("reply-recovers"), []byte("reply-recovers"))
	waitFor(t, 2*time.Second, "UDP reply clears rejection", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].Error == ""
	})
	if manager.Snapshot()[0].ErrorNetwork != "" || manager.Snapshot()[0].LastErrorNetwork != "udp" {
		t.Fatal("UDP recovery lost the rejection history network")
	}
}

func TestCurrentErrorNetworkFollowsLatestUnresolvedMixedError(t *testing.T) {
	var rt runtime
	rt.setUDPError(errors.New("older UDP failure"))
	rt.setTCPError(errors.New("newer TCP failure"))
	rt.tcpUpstreamEstablished()

	snapshot := runtimeSnapshot(&rt)
	if snapshot.Error != "older UDP failure" || snapshot.ErrorNetwork != "udp" {
		t.Fatalf("current error did not fall back to the unresolved UDP failure: %+v", snapshot)
	}
	if snapshot.LastError != "newer TCP failure" || snapshot.LastErrorNetwork != "tcp" {
		t.Fatalf("history fields did not retain the newer recovered TCP failure: %+v", snapshot)
	}
	if snapshot.LastRecoveryAt == "" {
		t.Fatal("TCP recovery did not record last_recovery_at")
	}

	rt.udpTargetReplied()
	snapshot = runtimeSnapshot(&rt)
	if snapshot.Error != "" || snapshot.ErrorNetwork != "" {
		t.Fatalf("cleared current error retained a network label: %+v", snapshot)
	}

	rt.setLifecycleError(errors.New("rule lifecycle failure"))
	snapshot = runtimeSnapshot(&rt)
	if snapshot.Error != "rule lifecycle failure" || snapshot.ErrorNetwork != "" {
		t.Fatalf("rule-level error was assigned a protocol: %+v", snapshot)
	}
}

func TestRecoveryIsIsolatedByTransportAndListenerFault(t *testing.T) {
	var rt runtime
	rt.setTCPError(errors.New("tcp dial failed"))
	rt.setUDPError(errors.New("udp response failed"))
	rt.tcpUpstreamEstablished()

	snapshot := runtimeSnapshot(&rt)
	if snapshot.Error != "udp response failed" ||
		snapshot.ErrorNetwork != "udp" ||
		snapshot.LastErrorNetwork != "udp" {
		t.Fatalf("TCP recovery cleared a UDP error: %+v", snapshot)
	}
	if snapshot.LastRecoveryAt == "" {
		t.Fatal("TCP recovery did not record last_recovery_at")
	}

	rt.setTCPListenerError(errors.New("accept tcp: listener stopped"))
	rt.setTCPError(errors.New("tcp target failed again"))
	rt.tcpUpstreamEstablished()
	snapshot = runtimeSnapshot(&rt)
	if !strings.Contains(snapshot.Error, "listener stopped") {
		t.Fatalf("TCP upstream success cleared a listener fault: %+v", snapshot)
	}
	if snapshot.ErrorNetwork != "tcp" {
		t.Fatalf("TCP listener fault lost its protocol label: %+v", snapshot)
	}
	if snapshot.LastError != "tcp target failed again" ||
		snapshot.LastErrorNetwork != "tcp" {
		t.Fatalf("listener fault replaced the separate error history: %+v", snapshot)
	}

	rt.udpTargetReplied()
	snapshot = runtimeSnapshot(&rt)
	if !strings.Contains(snapshot.Error, "listener stopped") {
		t.Fatalf("UDP reply cleared an unresolved TCP listener fault: %+v", snapshot)
	}
}

func TestRecoveryDoesNotMaskNewerConcurrentError(t *testing.T) {
	var rt runtime
	rt.setTCPError(errors.New("old failure"))
	observedBeforeNewError := rt.errorSequence.Load()

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		rt.setTCPError(errors.New("new failure"))
		close(done)
	}()
	<-started
	<-done

	// The successful handshake was observed before the new failure. Its
	// delayed cleanup must not clear that newer error.
	rt.clearRecoverableError("tcp", observedBeforeNewError)
	snapshot := runtimeSnapshot(&rt)
	if snapshot.Error != "new failure" || snapshot.LastError != "new failure" {
		t.Fatalf("delayed recovery masked a newer error: %+v", snapshot)
	}

	var wait sync.WaitGroup
	for i := 0; i < 256; i++ {
		wait.Add(2)
		go func(index int) {
			defer wait.Done()
			rt.setTCPError(fmt.Errorf("concurrent failure %d", index))
		}(i)
		go func() {
			defer wait.Done()
			rt.tcpUpstreamEstablished()
		}()
	}
	wait.Wait()

	rt.setTCPError(errors.New("final failure"))
	rt.tcpUpstreamEstablished()
	snapshot = runtimeSnapshot(&rt)
	if snapshot.Error != "" {
		t.Fatalf("final successful handshake did not clear its preceding error: %+v", snapshot)
	}
	if snapshot.LastError == "" {
		t.Fatal("concurrent recovery erased all error history")
	}
}

func TestApplyAndCopyPreserveHistoryWithoutCrossTargetErrors(t *testing.T) {
	listen := freeTCPAddress(t)
	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "history-reuse",
		Name:           "history reuse",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         freeTCPAddress(t),
		MaxConnections: 4,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	rt := runtimeForRule(t, manager, rule.ID)
	rt.setTCPError(errors.New("old target failed"))
	rt.tcpUpstreamEstablished()
	rt.setTCPListenerError(errors.New("old listener stopped"))
	before := manager.Snapshot()[0]
	if before.Error == "" || before.LastError != "old listener stopped" || before.LastRecoveryAt == "" {
		t.Fatalf("test setup did not create unresolved state: %+v", before)
	}

	compatible := rule
	compatible.Name = "renamed"
	if err := manager.Apply([]config.Rule{compatible}); err != nil {
		t.Fatalf("Apply compatible rule: %v", err)
	}
	reused := manager.Snapshot()[0]
	if !strings.Contains(reused.Error, "listener stopped") ||
		reused.LastError != before.LastError ||
		reused.LastErrorAt != before.LastErrorAt ||
		reused.LastRecoveryAt != before.LastRecoveryAt {
		t.Fatalf("compatible Apply lost error state or history: before=%+v after=%+v", before, reused)
	}

	changed := compatible
	changed.Target = freeTCPAddress(t)
	if err := manager.Apply([]config.Rule{changed}); err != nil {
		t.Fatalf("Apply changed target: %v", err)
	}
	replaced := manager.Snapshot()[0]
	if replaced.Error != "" {
		t.Fatalf("changed target inherited unresolved state: %+v", replaced)
	}
	if replaced.LastError != before.LastError ||
		replaced.LastErrorAt != before.LastErrorAt ||
		replaced.LastRecoveryAt != before.LastRecoveryAt ||
		replaced.LastErrorNetwork != before.LastErrorNetwork {
		t.Fatalf("changed target lost historical error information: before=%+v after=%+v", before, replaced)
	}

	var copied runtime
	copied.copyStatsFrom(rt)
	copiedSnapshot := runtimeSnapshot(&copied)
	if copiedSnapshot.Error != before.Error ||
		copiedSnapshot.LastError != before.LastError ||
		copiedSnapshot.LastErrorAt != before.LastErrorAt ||
		copiedSnapshot.LastRecoveryAt != before.LastRecoveryAt {
		t.Fatalf("copyStatsFrom lost error state or history: before=%+v copied=%+v", before, copiedSnapshot)
	}
	copied.tcpUpstreamEstablished()
	copiedSnapshot = runtimeSnapshot(&copied)
	if !strings.Contains(copiedSnapshot.Error, "listener stopped") ||
		copiedSnapshot.LastError != before.LastError {
		t.Fatalf("copied listener fault or history was not preserved: %+v", copiedSnapshot)
	}
}

func runtimeForRule(t *testing.T, manager *Manager, id string) *runtime {
	t.Helper()
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	rt := manager.runtime[ruleKey("id:"+id)]
	if rt == nil {
		t.Fatalf("runtime for rule %q not found", id)
	}
	return rt
}

func runtimeSnapshot(rt *runtime) Snapshot {
	var snapshot Snapshot
	rt.fillSnapshot(&snapshot)
	return snapshot
}

func parseSnapshotTime(t *testing.T, value string) time.Time {
	t.Helper()
	if value == "" {
		t.Fatal("timestamp is empty")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC3339: %v", value, err)
	}
	return parsed
}
