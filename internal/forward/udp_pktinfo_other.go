//go:build !linux

package forward

import (
	"net"
)

// Non-Linux builds keep the regular socket API. A specific bound address can
// still reply from that address, but wildcard IPv4/IPv6 listeners cannot
// reliably preserve the packet's local destination address or interface.
func enableUDPPacketInfo(_ *net.UDPConn, _ string) error {
	return nil
}

func udpControlBufferSize() int {
	return 0
}

func (e *udpEndpoint) readDatagram(buffer, _ []byte) (int, udpPacket, error) {
	n, client, err := e.conn.ReadFromUDPAddrPort(buffer)
	if err != nil {
		return 0, udpPacket{}, err
	}
	packet := udpPacket{
		client: normalizeClientAddr(client),
		local:  e.configuredLocalAddress(),
	}
	if packet.local.IsValid() {
		if packet.local.Addr().Is4() {
			packet.pktinfoFamily = udpPacketInfoIPv4
		} else {
			packet.pktinfoFamily = udpPacketInfoIPv6
		}
	}
	return n, packet, nil
}

func (e *udpEndpoint) writeDatagram(payload []byte, session *udpSession) error {
	_, err := e.conn.WriteToUDPAddrPort(payload, session.client)
	return err
}
