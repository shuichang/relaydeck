package forward

import (
	"io"
	"net"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

// Run with -run '^$' -bench BenchmarkTCPIdleConnections -benchtime=1500x.
// The reported retained heap includes relay, client, and echo-server sockets
// in one process; compare identical connection counts on the same machine.
func BenchmarkTCPIdleConnections(b *testing.B) {
	if b.N > 4096 {
		b.Skip("use a fixed connection count, for example -benchtime=1500x")
	}
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer target.Close()
	var handlers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	port, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	address := port.Addr().String()
	_ = port.Close()
	manager := NewManager()
	defer func() {
		manager.Close()
		_ = target.Close()
		<-acceptDone
		handlers.Wait()
	}()
	if err := manager.Apply([]config.Rule{{
		ID: "memory-bench", Enabled: true, Network: "tcp",
		Listen: address, Target: target.Addr().String(), MaxConnections: 4096,
	}}); err != nil {
		b.Fatal(err)
	}
	connections := make([]net.Conn, 0, b.N)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	goruntime.GC()
	var before, after goruntime.MemStats
	goruntime.ReadMemStats(&before)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout("tcp4", address, 3*time.Second)
		if err != nil {
			b.Fatalf("connection %d: %v", i, err)
		}
		connections = append(connections, conn)
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte{42}); err != nil {
			b.Fatal(err)
		}
		var echo [1]byte
		if _, err := io.ReadFull(conn, echo[:]); err != nil || echo[0] != 42 {
			b.Fatalf("connection %d echo: %v", i, err)
		}
		_ = conn.SetDeadline(time.Time{})
	}
	b.StopTimer()
	goruntime.GC()
	goruntime.ReadMemStats(&after)
	if rows := manager.Snapshot(); len(rows) != 1 || rows[0].ActiveConnections != int64(b.N) {
		b.Fatalf("not all benchmark connections remained live: %+v", rows)
	}
	if after.HeapAlloc > before.HeapAlloc {
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/float64(b.N), "live_heap_B/conn")
	}
	b.ReportMetric(float64(b.N), "connections")
}
