package forward

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

func TestConnectionLimitDefaultsExplicitValuesAndMaximum(t *testing.T) {
	if config.DefaultMaxConnections != 8192 {
		t.Fatalf("DefaultMaxConnections = %d, want 8192", config.DefaultMaxConnections)
	}
	if config.MaxConnections != 65536 {
		t.Fatalf("MaxConnections = %d, want 65536", config.MaxConnections)
	}

	tests := []struct {
		name       string
		configured int
		effective  int
	}{
		{name: "default zero", configured: 0, effective: config.DefaultMaxConnections},
		{name: "legacy explicit value", configured: 256, effective: 256},
		{name: "above previous maximum", configured: 4097, effective: 4097},
		{name: "new maximum", configured: config.MaxConnections, effective: config.MaxConnections},
	}

	rules := make([]config.Rule, len(tests))
	for i, test := range tests {
		rules[i] = config.Rule{
			ID:             fmt.Sprintf("limit-%d", i),
			Name:           test.name,
			Enabled:        false,
			Network:        "tcp",
			Listen:         "127.0.0.1:12000",
			Target:         "192.0.2.1:443",
			MaxConnections: test.configured,
		}
	}

	manager := NewManager()
	t.Cleanup(manager.Close)
	if err := manager.Apply(rules); err != nil {
		t.Fatalf("Apply limits: %v", err)
	}
	snapshots := manager.Snapshot()
	if len(snapshots) != len(tests) {
		t.Fatalf("Snapshot returned %d rules, want %d", len(snapshots), len(tests))
	}
	for i, test := range tests {
		if snapshots[i].MaxConnections != test.configured {
			t.Errorf("%s: configured max_connections = %d, want %d",
				test.name, snapshots[i].MaxConnections, test.configured)
		}
		if snapshots[i].EffectiveMaxConnections != test.effective {
			t.Errorf("%s: effective_max_connections = %d, want %d",
				test.name, snapshots[i].EffectiveMaxConnections, test.effective)
		}
	}

	for _, test := range []struct {
		name  string
		value int
	}{
		{name: "negative", value: -1},
		{name: "above maximum", value: config.MaxConnections + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			rule := rules[0]
			rule.MaxConnections = test.value
			err := ValidateRules([]config.Rule{rule})
			if err == nil || !strings.Contains(err.Error(), "max_connections") {
				t.Fatalf("ValidateRules() error = %v, want max_connections error", err)
			}
		})
	}
}

func TestDefaultLimitAllows512ConcurrentTCPEchoSessions(t *testing.T) {
	target, accepted := startTCPEcho(t, "127.0.0.1:0")
	listen := freeTCPAddress(t)

	manager := NewManager()
	t.Cleanup(manager.Close)
	rule := config.Rule{
		ID:             "default-concurrency",
		Name:           "default concurrency",
		Enabled:        true,
		Network:        "tcp",
		Listen:         listen,
		Target:         target,
		MaxConnections: 0,
	}
	if err := manager.Apply([]config.Rule{rule}); err != nil {
		t.Fatalf("Apply default connection limit: %v", err)
	}

	const sessions = 512
	const batchSize = 32
	type result struct {
		conn *net.TCPConn
		err  error
	}

	connections := make([]*net.TCPConn, 0, sessions)
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	for start := 0; start < sessions; start += batchSize {
		end := start + batchSize
		if end > sessions {
			end = sessions
		}
		results := make(chan result, end-start)
		var wait sync.WaitGroup
		for i := start; i < end; i++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()

				payload := []byte(fmt.Sprintf("session-%03d", index))
				conn, err := net.DialTimeout("tcp4", listen, 10*time.Second)
				if err != nil {
					results <- result{err: fmt.Errorf("session %d dial: %w", index, err)}
					return
				}
				tcp, ok := conn.(*net.TCPConn)
				if !ok {
					_ = conn.Close()
					results <- result{err: fmt.Errorf("session %d returned %T, want *net.TCPConn", index, conn)}
					return
				}
				_ = tcp.SetDeadline(time.Now().Add(20 * time.Second))
				if _, err = tcp.Write(payload); err == nil {
					echo := make([]byte, len(payload))
					_, err = io.ReadFull(tcp, echo)
					if err == nil && string(echo) != string(payload) {
						err = fmt.Errorf("echo = %q, want %q", echo, payload)
					}
				}
				if err != nil {
					_ = tcp.Close()
					results <- result{err: fmt.Errorf("session %d: %w", index, err)}
					return
				}
				results <- result{conn: tcp}
			}(i)
		}
		wait.Wait()
		close(results)

		for result := range results {
			if result.err != nil {
				t.Error(result.err)
				continue
			}
			connections = append(connections, result.conn)
		}
		if t.Failed() {
			t.Fatalf("only %d of %d connections preserved their echo after batch ending at %d",
				len(connections), sessions, end)
		}
	}

	waitFor(t, 5*time.Second, "512 admitted TCP connections", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 &&
			snapshot[0].ActiveConnections == sessions &&
			accepted.Load() == sessions
	})
	snapshot := manager.Snapshot()[0]
	if snapshot.EffectiveMaxConnections != config.DefaultMaxConnections {
		t.Fatalf("effective_max_connections = %d, want %d",
			snapshot.EffectiveMaxConnections, config.DefaultMaxConnections)
	}

	for _, conn := range connections {
		_ = conn.Close()
	}
	waitFor(t, 5*time.Second, "TCP connection cleanup", func() bool {
		snapshot := manager.Snapshot()
		return len(snapshot) == 1 && snapshot[0].ActiveConnections == 0
	})
}
