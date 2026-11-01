package forward

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

var errUDPPacketInfoMissing = errors.New("UDP local destination information is missing")

type udpEndpoint struct {
	runtime *runtime
	conn    *net.UDPConn
	network string

	stopOnce sync.Once
	started  atomic.Bool
	runningV atomic.Bool
	done     chan struct{}
	stop     chan struct{}

	mu        sync.Mutex
	closing   bool
	sessions  map[udpClientKey]*udpSession
	sessionWG sync.WaitGroup
}

// udpPacket retains the local destination and interface reported by the
// Linux packet-info control message so a wildcard listener can select the
// correct reply source. Other platforms populate only the configured address.
type udpPacket struct {
	client        netip.AddrPort
	local         netip.AddrPort
	ifindex       uint32
	pktinfoFamily uint8
}

const (
	udpPacketInfoNone uint8 = iota
	udpPacketInfoIPv4
	udpPacketInfoIPv6
)

type udpClientKey struct {
	client  netip.AddrPort
	local   netip.AddrPort
	ifindex uint32
	family  uint8
}

type udpSession struct {
	runtime  *runtime
	endpoint *udpEndpoint
	key      udpClientKey
	client   netip.AddrPort
	local    netip.AddrPort
	ifindex  uint32
	family   uint8
	conn     *net.UDPConn
	lastSeen atomic.Int64
}

func newUDPEndpoint(rt *runtime) (*udpEndpoint, error) {
	spec := rt.spec.listen
	network := listenNetwork(spec, "udp")
	addr := &net.UDPAddr{
		IP:   net.IP(spec.addr.Addr().AsSlice()),
		Port: int(spec.addr.Port()),
		Zone: spec.addr.Addr().Zone(),
	}
	conn, err := net.ListenUDP(network, addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %s: %w", spec.canonical, err)
	}
	if err := enableUDPPacketInfo(conn, network); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable UDP packet info for %s: %w", spec.canonical, err)
	}
	return &udpEndpoint{
		runtime:  rt,
		conn:     conn,
		network:  network,
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
		sessions: make(map[udpClientKey]*udpSession),
	}, nil
}

func (e *udpEndpoint) start() {
	if !e.started.CompareAndSwap(false, true) {
		return
	}
	e.runningV.Store(true)
	go e.run()
}

func (e *udpEndpoint) running() bool {
	return e.runningV.Load()
}

func (e *udpEndpoint) close() {
	e.stopOnce.Do(func() {
		e.mu.Lock()
		e.closing = true
		e.mu.Unlock()
		close(e.stop)
		_ = e.conn.Close()

		e.mu.Lock()
		sessions := make([]*udpSession, 0, len(e.sessions))
		for _, session := range e.sessions {
			sessions = append(sessions, session)
		}
		e.mu.Unlock()
		for _, session := range sessions {
			_ = session.conn.Close()
		}
		if e.started.Load() {
			<-e.done
		}
	})
}

func (e *udpEndpoint) run() {
	defer e.runningV.Store(false)
	defer close(e.done)

	cleanupDone := make(chan struct{})
	cleanupStop := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		e.cleanupLoop(cleanupStop)
	}()

	buffer := make([]byte, 65535)
	oob := make([]byte, udpControlBufferSize())
	for {
		n, packet, err := e.readDatagram(buffer, oob)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			e.runtime.setError(fmt.Errorf("read udp: %w", err))
			if errors.Is(err, errUDPPacketInfoMissing) {
				continue
			}
			break
		}
		if !e.runtime.clientAllowed(packet.client.Addr()) {
			e.runtime.setError(fmt.Errorf("rejected udp source %s: source is not allowed", packet.client))
			continue
		}
		e.forwardDatagram(packet, buffer[:n])
	}

	_ = e.conn.Close()
	close(cleanupStop)
	e.mu.Lock()
	sessions := make([]*udpSession, 0, len(e.sessions))
	for _, session := range e.sessions {
		sessions = append(sessions, session)
	}
	e.mu.Unlock()
	for _, session := range sessions {
		_ = session.conn.Close()
	}
	<-cleanupDone
	e.sessionWG.Wait()
}

func (e *udpEndpoint) forwardDatagram(packet udpPacket, payload []byte) {
	packet.client = normalizeClientAddr(packet.client)
	key := udpClientKey{
		client:  packet.client,
		local:   packet.local,
		ifindex: packet.ifindex,
		family:  packet.pktinfoFamily,
	}

	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return
	}
	session := e.sessions[key]
	if session == nil {
		if len(e.sessions) >= e.runtime.spec.maxConns {
			e.mu.Unlock()
			e.runtime.setError(fmt.Errorf("udp session limit reached (%d)", e.runtime.spec.maxConns))
			return
		}
		target := netip.AddrPortFrom(e.runtime.spec.target.addr, e.runtime.spec.target.port)
		conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(target))
		if err != nil {
			e.mu.Unlock()
			e.runtime.setError(fmt.Errorf("dial udp target: %w", err))
			return
		}
		now := time.Now().UnixNano()
		session = &udpSession{
			runtime:  e.runtime,
			endpoint: e,
			key:      key,
			client:   packet.client,
			local:    packet.local,
			ifindex:  packet.ifindex,
			family:   packet.pktinfoFamily,
			conn:     conn,
		}
		session.lastSeen.Store(now)
		e.sessions[key] = session
		e.runtime.udpSessions.Store(int64(len(e.sessions)))
		e.sessionWG.Add(1)
		go e.receiveSession(session)
	} else {
		session.lastSeen.Store(time.Now().UnixNano())
	}
	e.mu.Unlock()

	_ = session.conn.SetWriteDeadline(time.Now().Add(udpWriteTimeout))
	if _, err := session.conn.Write(payload); err != nil {
		e.runtime.setError(fmt.Errorf("write udp target: %w", err))
		e.removeSession(session)
		return
	}
	e.runtime.bytesIn.Add(uint64(len(payload)))
	session.lastSeen.Store(time.Now().UnixNano())
}

func (e *udpEndpoint) receiveSession(session *udpSession) {
	defer func() {
		_ = session.conn.Close()
		e.removeSession(session)
		e.sessionWG.Done()
	}()

	buffer := make([]byte, 65535)
	for {
		n, _, err := session.conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				e.runtime.setError(fmt.Errorf("read udp target: %w", err))
			}
			return
		}
		session.lastSeen.Store(time.Now().UnixNano())

		err = e.writeDatagram(buffer[:n], session)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				e.runtime.setError(fmt.Errorf("write udp client: %w", err))
				return
			}
			return
		}
		e.runtime.bytesOut.Add(uint64(n))
	}
}

func normalizeClientAddr(client netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(client.Addr().Unmap().WithZone(client.Addr().Zone()), client.Port())
}

func (e *udpEndpoint) configuredLocalAddress() netip.AddrPort {
	listen := e.runtime.spec.listen.addr
	if listen.Addr().IsUnspecified() {
		return netip.AddrPort{}
	}
	return listen
}

func (e *udpEndpoint) removeSession(session *udpSession) {
	e.mu.Lock()
	if current := e.sessions[session.key]; current == session {
		delete(e.sessions, session.key)
		e.runtime.udpSessions.Store(int64(len(e.sessions)))
	}
	e.mu.Unlock()
	_ = session.conn.Close()
}

func (e *udpEndpoint) cleanupLoop(cleanupStop <-chan struct{}) {
	timeout := e.runtime.spec.udpTimeout
	interval := timeout / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			cutoff := now.Add(-timeout).UnixNano()
			e.mu.Lock()
			var expired []*udpSession
			for key, session := range e.sessions {
				if session.lastSeen.Load() <= cutoff {
					delete(e.sessions, key)
					expired = append(expired, session)
				}
			}
			e.runtime.udpSessions.Store(int64(len(e.sessions)))
			e.mu.Unlock()
			for _, session := range expired {
				_ = session.conn.Close()
			}
		case <-e.stop:
			return
		case <-cleanupStop:
			return
		}
	}
}
