package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

type tcpEndpoint struct {
	runtime  *runtime
	listener *net.TCPListener
	ctx      context.Context
	cancel   context.CancelFunc

	stopOnce sync.Once
	done     chan struct{}
	started  atomic.Bool
	runningV atomic.Bool

	mu      sync.Mutex
	closing bool
	conns   map[*net.TCPConn]struct{}
	connWG  sync.WaitGroup
	active  int
}

var tcpCopyBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, copyBufferSize)
		return &buffer
	},
}

func newTCPEndpoint(rt *runtime) (*tcpEndpoint, error) {
	spec := rt.spec.listen
	network := listenNetwork(spec, "tcp")
	addr := &net.TCPAddr{
		IP:   net.IP(spec.addr.Addr().AsSlice()),
		Port: int(spec.addr.Port()),
		Zone: spec.addr.Addr().Zone(),
	}
	listener, err := net.ListenTCP(network, addr)
	if err != nil {
		return nil, fmt.Errorf("listen tcp %s: %w", spec.canonical, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &tcpEndpoint{
		runtime:  rt,
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		conns:    make(map[*net.TCPConn]struct{}),
	}, nil
}

func (e *tcpEndpoint) start() {
	if !e.started.CompareAndSwap(false, true) {
		return
	}
	e.runningV.Store(true)
	go e.acceptLoop()
}

func (e *tcpEndpoint) running() bool {
	return e.runningV.Load()
}

func (e *tcpEndpoint) close() {
	e.stopOnce.Do(func() {
		e.cancel()
		_ = e.listener.Close()

		e.mu.Lock()
		e.closing = true
		conns := make([]*net.TCPConn, 0, len(e.conns))
		for conn := range e.conns {
			conns = append(conns, conn)
		}
		e.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		if e.started.Load() {
			<-e.done
			e.connWG.Wait()
		}
	})
}

func (e *tcpEndpoint) acceptLoop() {
	defer e.runningV.Store(false)
	defer close(e.done)

	backoff := 5 * time.Millisecond
	for {
		conn, err := e.listener.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || e.ctx.Err() != nil {
				break
			}
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				e.runtime.setTCPError(fmt.Errorf("accept tcp: %w", err))
				timer := time.NewTimer(backoff)
				select {
				case <-e.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if backoff < time.Second {
					backoff *= 2
				}
				continue
			}
			e.runtime.setTCPListenerError(fmt.Errorf("accept tcp: %w", err))
			break
		}
		backoff = 5 * time.Millisecond
		if !e.admit(conn) {
			_ = conn.Close()
		}
	}
}

func (e *tcpEndpoint) admit(conn *net.TCPConn) bool {
	remote, ok := addrFromNetAddr(conn.RemoteAddr())
	if !ok || !e.runtime.clientAllowed(remote.Addr()) {
		e.runtime.setTCPError(fmt.Errorf("rejected connection from %s: source is not allowed", conn.RemoteAddr()))
		return false
	}

	e.mu.Lock()
	if e.closing || e.active >= e.runtime.spec.maxConns {
		e.mu.Unlock()
		e.runtime.setTCPError(fmt.Errorf("connection limit reached (%d)", e.runtime.spec.maxConns))
		return false
	}
	e.active++
	e.conns[conn] = struct{}{}
	e.runtime.activeConnections.Store(int64(e.active))
	e.connWG.Add(1)
	e.mu.Unlock()

	go e.handle(conn)
	return true
}

func (e *tcpEndpoint) handle(client *net.TCPConn) {
	defer func() {
		_ = client.Close()
		e.mu.Lock()
		delete(e.conns, client)
		e.active--
		e.runtime.activeConnections.Store(int64(e.active))
		e.mu.Unlock()
		e.connWG.Done()
	}()

	dialer := net.Dialer{
		Timeout:   targetDialTimeout,
		KeepAlive: 30 * time.Second,
	}
	upstream, err := dialer.DialContext(e.ctx, "tcp4", e.runtime.spec.target.String())
	if err != nil {
		e.runtime.setTCPError(fmt.Errorf("dial target: %w", err))
		return
	}
	upstreamTCP, ok := upstream.(*net.TCPConn)
	if !ok {
		_ = upstream.Close()
		e.runtime.setTCPError(errors.New("tcp4 dial returned a non-TCP connection"))
		return
	}
	defer upstreamTCP.Close()
	e.runtime.tcpUpstreamEstablished()

	proxyTCP(e.ctx, e.runtime, client, upstreamTCP)
}

type copyResult struct {
	err error
}

func proxyTCP(ctx context.Context, rt *runtime, client, upstream *net.TCPConn) {
	stopCancelClose := context.AfterFunc(ctx, func() {
		_ = client.Close()
		_ = upstream.Close()
	})
	defer stopCancelClose()

	results := make(chan copyResult, 2)
	go copyTCPHalf(&rt.bytesIn, results, upstream, client)
	go copyTCPHalf(&rt.bytesOut, results, client, upstream)

	first := <-results
	if meaningfulCopyError(first.err) {
		_ = client.Close()
		_ = upstream.Close()
	}
	second := <-results
	if meaningfulCopyError(second.err) {
		_ = client.Close()
		_ = upstream.Close()
	}
}

type countingTCPWriter struct {
	conn    *net.TCPConn
	counter *atomic.Uint64
}

func (w countingTCPWriter) Write(payload []byte) (int, error) {
	n, err := w.conn.Write(payload)
	if n > 0 {
		w.counter.Add(uint64(n))
	}
	return n, err
}

func copyTCPHalf(counter *atomic.Uint64, results chan<- copyResult, dst, src *net.TCPConn) {
	buffer := tcpCopyBufferPool.Get().(*[]byte)
	defer tcpCopyBufferPool.Put(buffer)

	// TCPConn.WriteTo would bypass CopyBuffer's buffer and, because our
	// counting writer cannot use splice, allocate another 32 KiB per direction.
	// Hide WriterTo so every stream really uses the bounded pooled buffer.
	_, err := io.CopyBuffer(countingTCPWriter{conn: dst, counter: counter}, struct{ io.Reader }{src}, *buffer)
	_ = dst.CloseWrite()
	results <- copyResult{err: err}
}

func meaningfulCopyError(err error) bool {
	return err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF)
}

func listenNetwork(spec listenSpec, base string) string {
	addr := spec.addr.Addr()
	if addr.Is4() {
		return base + "4"
	}
	if addr.IsUnspecified() {
		return base
	}
	return base + "6"
}

func addrFromNetAddr(addr net.Addr) (netip.AddrPort, bool) {
	switch value := addr.(type) {
	case *net.TCPAddr:
		parsed, ok := netip.AddrFromSlice(value.IP)
		if !ok {
			return netip.AddrPort{}, false
		}
		parsed = parsed.Unmap().WithZone(value.Zone)
		return netip.AddrPortFrom(parsed, uint16(value.Port)), true
	case *net.UDPAddr:
		parsed, ok := netip.AddrFromSlice(value.IP)
		if !ok {
			return netip.AddrPort{}, false
		}
		parsed = parsed.Unmap().WithZone(value.Zone)
		return netip.AddrPortFrom(parsed, uint16(value.Port)), true
	default:
		return netip.AddrPort{}, false
	}
}
