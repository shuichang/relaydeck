//go:build linux

package forward

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"unsafe"
)

func enableUDPPacketInfo(conn *net.UDPConn, network string) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		if network == "udp4" {
			socketErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_PKTINFO, 1)
			return
		}
		socketErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_RECVPKTINFO, 1)
		if socketErr == nil && network == "udp" {
			// A dual-stack socket may report IPv4 packets through either
			// control-message family. IP_PKTINFO is best effort here.
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_PKTINFO, 1)
		}
	}); err != nil {
		return err
	}
	return socketErr
}

func udpControlBufferSize() int {
	return syscall.CmsgSpace(syscall.SizeofInet4Pktinfo) +
		syscall.CmsgSpace(syscall.SizeofInet6Pktinfo)
}

func (e *udpEndpoint) readDatagram(buffer, oob []byte) (int, udpPacket, error) {
	n, oobn, flags, remote, err := e.conn.ReadMsgUDP(buffer, oob)
	if err != nil {
		return 0, udpPacket{}, err
	}
	if flags&syscall.MSG_CTRUNC != 0 {
		return 0, udpPacket{}, errors.New("UDP control message was truncated")
	}

	client, ok := addrFromNetAddr(remote)
	if !ok {
		return 0, udpPacket{}, fmt.Errorf("invalid UDP client address %v", remote)
	}
	packet := udpPacket{client: normalizeClientAddr(client)}

	controlMessages, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return 0, udpPacket{}, fmt.Errorf("parse UDP control message: %w", err)
	}
	var (
		local4, local6 netip.Addr
		ifindex4       uint32
		ifindex6       uint32
	)
	for _, message := range controlMessages {
		switch {
		case message.Header.Level == syscall.IPPROTO_IP &&
			message.Header.Type == syscall.IP_PKTINFO:
			if len(message.Data) < syscall.SizeofInet4Pktinfo {
				return 0, udpPacket{}, errors.New("short IPv4 packet information")
			}
			ifindex := binary.NativeEndian.Uint32(message.Data[0:4])
			specDst := netip.AddrFrom4([4]byte{
				message.Data[4], message.Data[5], message.Data[6], message.Data[7],
			})
			headerDst := netip.AddrFrom4([4]byte{
				message.Data[8], message.Data[9], message.Data[10], message.Data[11],
			})
			local := specDst
			if local.IsUnspecified() {
				local = headerDst
			}
			if !local.IsValid() || local.IsUnspecified() {
				continue
			}
			local4 = local
			ifindex4 = ifindex

		case message.Header.Level == syscall.IPPROTO_IPV6 &&
			message.Header.Type == syscall.IPV6_PKTINFO:
			if len(message.Data) < syscall.SizeofInet6Pktinfo {
				return 0, udpPacket{}, errors.New("short IPv6 packet information")
			}
			rawAddr := [16]byte{}
			copy(rawAddr[:], message.Data[:16])
			local := netip.AddrFrom16(rawAddr).Unmap()
			if !local.IsValid() || local.IsUnspecified() {
				continue
			}
			local6 = local
			ifindex6 = binary.NativeEndian.Uint32(message.Data[16:20])
		}
	}

	switch e.network {
	case "udp4":
		if !local4.IsValid() {
			break
		}
		packet.local = netip.AddrPortFrom(local4, e.runtime.spec.listen.addr.Port())
		packet.ifindex = ifindex4
		packet.pktinfoFamily = udpPacketInfoIPv4
	case "udp6":
		if !local6.IsValid() {
			break
		}
		packet.local = netip.AddrPortFrom(local6, e.runtime.spec.listen.addr.Port())
		packet.ifindex = ifindex6
		packet.pktinfoFamily = udpPacketInfoIPv6
	default:
		if local6.IsValid() {
			// An IPv6 socket reports IPv4-mapped destinations as IPv6 packet
			// information. Prefer that family when both controls are present.
			packet.local = netip.AddrPortFrom(local6, e.runtime.spec.listen.addr.Port())
			packet.ifindex = ifindex6
			packet.pktinfoFamily = udpPacketInfoIPv6
		} else if local4.IsValid() {
			packet.local = netip.AddrPortFrom(local4, e.runtime.spec.listen.addr.Port())
			packet.ifindex = ifindex4
			packet.pktinfoFamily = udpPacketInfoIPv4
		}
	}

	if !packet.local.IsValid() || packet.local.Addr().IsUnspecified() {
		packet.local = e.configuredLocalAddress()
		if !packet.local.IsValid() || packet.local.Addr().IsUnspecified() {
			return 0, udpPacket{}, errUDPPacketInfoMissing
		}
		if packet.pktinfoFamily == udpPacketInfoNone {
			if packet.local.Addr().Is4() {
				packet.pktinfoFamily = udpPacketInfoIPv4
			} else {
				packet.pktinfoFamily = udpPacketInfoIPv6
			}
		}
	}
	return n, packet, nil
}

func (e *udpEndpoint) writeDatagram(payload []byte, session *udpSession) error {
	if !session.local.IsValid() ||
		session.local.Addr().IsUnspecified() ||
		session.family == udpPacketInfoNone {
		_, err := e.conn.WriteToUDPAddrPort(payload, session.client)
		return err
	}

	oob, err := buildPacketInfoOOB(session.family, session.local.Addr(), session.ifindex)
	if err != nil {
		return err
	}
	_, _, err = e.conn.WriteMsgUDP(payload, oob, net.UDPAddrFromAddrPort(session.client))
	return err
}

func buildPacketInfoOOB(family uint8, local netip.Addr, ifindex uint32) ([]byte, error) {
	var dataLength int
	var level, messageType int
	switch family {
	case udpPacketInfoIPv4:
		dataLength = syscall.SizeofInet4Pktinfo
		level = syscall.IPPROTO_IP
		messageType = syscall.IP_PKTINFO
	case udpPacketInfoIPv6:
		dataLength = syscall.SizeofInet6Pktinfo
		level = syscall.IPPROTO_IPV6
		messageType = syscall.IPV6_PKTINFO
	default:
		return nil, errors.New("unknown UDP packet information family")
	}

	oob := make([]byte, syscall.CmsgSpace(dataLength))
	header := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
	header.Level = int32(level)
	header.Type = int32(messageType)
	setCmsgLength(header, syscall.CmsgLen(dataLength))

	data := oob[syscall.CmsgLen(0):]
	if family == udpPacketInfoIPv4 {
		if !local.Unmap().Is4() {
			return nil, errors.New("IPv4 packet information has a non-IPv4 source")
		}
		addr := local.Unmap().As4()
		binary.NativeEndian.PutUint32(data[0:4], ifindex)
		copy(data[4:8], addr[:])
		copy(data[8:12], addr[:])
	} else {
		addr := local.As16()
		copy(data[:16], addr[:])
		binary.NativeEndian.PutUint32(data[16:20], ifindex)
	}
	return oob, nil
}

// Cmsghdr.Len is uint64 on 64-bit Linux and uint32 on 32-bit Linux. The
// assignment is width-sensitive, so set the native field according to its
// compile-time size rather than assuming amd64.
func setCmsgLength(header *syscall.Cmsghdr, length int) {
	if unsafe.Sizeof(header.Len) == 8 {
		*(*uint64)(unsafe.Pointer(&header.Len)) = uint64(length)
		return
	}
	*(*uint32)(unsafe.Pointer(&header.Len)) = uint32(length)
}
