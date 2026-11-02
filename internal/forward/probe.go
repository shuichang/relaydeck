package forward

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

// ProbeEndpoint describes one transport check. A successful TCP handshake does
// not verify the application protocol, authentication, or UDP connectivity.
type ProbeEndpoint struct {
	Status    string   `json:"status"`
	LatencyMS *float64 `json:"latency_ms"`
	Message   string   `json:"message"`
	UDPStatus string   `json:"udp_status,omitempty"`
}

type ProbeResult struct {
	CheckedAt     time.Time     `json:"checked_at"`
	TargetAddress string        `json:"target_address"`
	Listen        ProbeEndpoint `json:"listen"`
	Target        ProbeEndpoint `json:"target"`
}

// Probe checks a draft without applying or persisting it. All network work is
// bounded by ctx. Listening and target checks run independently; an unresolvable
// target must not hide a port conflict at the ingress.
func (m *Manager) Probe(ctx context.Context, rule config.Rule) (ProbeResult, error) {
	if err := ValidateRules([]config.Rule{rule}); err != nil {
		return ProbeResult{}, err
	}
	listen, _ := parseListen(rule.Listen)
	result := ProbeResult{CheckedAt: time.Now().UTC()}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		result.Listen = m.probeListen(ctx, rule, listen)
	}()
	go func() {
		defer wg.Done()
		result.TargetAddress, result.Target = probeTarget(ctx, rule)
	}()
	wg.Wait()
	return result, nil
}

func (m *Manager) probeListen(ctx context.Context, rule config.Rule, listen listenSpec) ProbeEndpoint {
	if err := ctx.Err(); err != nil {
		return ProbeEndpoint{Status: "error", Message: "检测已取消或超时"}
	}
	// Temporary bind checks must not steal a port between Apply stopping an
	// old listener and binding its replacement or rollback. Unlike Lock, the
	// wait here is cancelable and remains inside the request's probe budget.
	if err := m.lockApplyForProbe(ctx); err != nil {
		return ProbeEndpoint{Status: "error", Message: "等待规则更新完成时检测已取消或超时"}
	}
	defer m.applyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return ProbeEndpoint{Status: "error", Message: "检测已取消或超时"}
	}
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return ProbeEndpoint{Status: "error", Message: "转发服务已关闭"}
	}
	var tcpOwned, udpOwned bool
	for _, rt := range m.runtime {
		// Only the edited rule may reuse its port. A new rule using another
		// rule's port must be reported as a conflict.
		if rule.ID == "" || rt.spec.rule.ID != rule.ID || rt.spec.listen.canonical != listen.canonical {
			continue
		}
		tcpOwned = rt.tcp != nil && rt.tcp.running()
		udpOwned = rt.udp != nil && rt.udp.running()
	}
	m.mu.RUnlock()

	parts := make([]string, 0, 3)
	result := ProbeEndpoint{Status: "listening"}
	if hasTransport(rule.Network, "tcp") {
		if tcpOwned {
			// Connecting to our own listener would create a real forwarding
			// session, potentially lingering after a half-close to a silent
			// upstream. Inspect ownership without spending a client slot.
			parts = append(parts, "本机 TCP 监听正常；入口的真实网络延迟须由客户端测量")
		} else {
			lc := net.ListenConfig{}
			listener, err := lc.Listen(ctx, listenNetwork(listen, "tcp"), listen.canonical)
			if err != nil {
				return ProbeEndpoint{Status: "error", Message: fmt.Sprintf("TCP 监听地址不可绑定：%v", err)}
			}
			defer listener.Close()
			result.Status = "available"
			parts = append(parts, "TCP 端口可绑定；保存启用后才会转发")
		}
	}
	if hasTransport(rule.Network, "udp") {
		if udpOwned {
			parts = append(parts, "本机 UDP 正在监听；未测量 UDP 往返")
		} else {
			lc := net.ListenConfig{}
			conn, err := lc.ListenPacket(ctx, listenNetwork(listen, "udp"), listen.canonical)
			if err != nil {
				return ProbeEndpoint{Status: "error", Message: fmt.Sprintf("UDP 监听地址不可绑定：%v", err)}
			}
			defer conn.Close()
			result.Status = "available"
			parts = append(parts, "UDP 端口可绑定；保存启用后才会转发")
		}
		result.UDPStatus = "unverified"
	}
	parts = append(parts, "入口检测仅在本机执行，不代表公网防火墙、客户端链路或协议认证已通过")
	if len(rule.AllowedCIDRs) > 0 {
		parts = append(parts, "已设置来源白名单，客户端还须符合 CIDR 限制")
	}
	result.Message = strings.Join(parts, "；")
	return result
}

func (m *Manager) lockApplyForProbe(ctx context.Context) error {
	if m.applyMu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if m.applyMu.TryLock() {
				return nil
			}
		}
	}
}

func probeTarget(ctx context.Context, rule config.Rule) (string, ProbeEndpoint) {
	// DNS time is excluded from latency_ms: the metric is a TCP handshake to
	// the resolved IPv4 endpoint, not an application or tunnel round trip.
	target, err := resolveTarget(ctx, rule.Target)
	if err != nil {
		return "", ProbeEndpoint{Status: "error", Message: fmt.Sprintf("目标 IPv4 解析失败：%v", err)}
	}
	address := target.String()
	listen, _ := parseListen(rule.Listen)
	if canLoopBackToListener(listen, target) {
		locals, localErr := localAddressSet()
		if localErr != nil {
			return address, ProbeEndpoint{Status: "error", Message: fmt.Sprintf("检查本机地址失败：%v", localErr)}
		}
		if _, local := locals[target.addr.Unmap().WithZone("")]; local || target.addr.IsLoopback() {
			return address, ProbeEndpoint{Status: "error", Message: "目标会回到规则自己的监听端口，形成转发环路"}
		}
	}
	if rule.Network == "udp" {
		return address, ProbeEndpoint{
			Status: "unverified", UDPStatus: "unverified",
			Message: "目标 IPv4 已解析；UDP 没有通用握手，未发送无意义的测试报文。请用真实协议请求及双向流量验证，不能据此判定端口已接通",
		}
	}
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return address, ProbeEndpoint{Status: "error", Message: fmt.Sprintf("本机到目标 TCP 握手失败：%v", err)}
	}
	// A probe never sends application data. Abort instead of half-closing so
	// a forwarding target can promptly tear down its own upstream session.
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
	result := ProbeEndpoint{
		Status: "reachable", LatencyMS: elapsedMS(start),
		Message: "本机到目标 IPv4 的 TCP 握手成功；延迟不含 DNS，尚未验证应用协议、认证或隧道内的最终目标",
	}
	if rule.Network == "both" {
		result.UDPStatus = "unverified"
		result.Message += "；UDP 尚未验证，需真实协议双向流量确认"
	}
	return address, result
}

func elapsedMS(start time.Time) *float64 {
	ms := float64(time.Since(start)) / float64(time.Millisecond)
	return &ms
}
