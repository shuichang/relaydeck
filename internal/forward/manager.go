package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

const (
	defaultUDPTimeout = 60 * time.Second
	maxUDPTimeout     = 24 * time.Hour
	maxRuleCount      = 512
	maxRuleIDBytes    = 128
	maxRuleNameBytes  = 256
	maxAllowedCIDRs   = 128

	targetResolveTimeout = 5 * time.Second
	targetDialTimeout    = 10 * time.Second
	udpWriteTimeout      = 2 * time.Second
	copyBufferSize       = 8 * 1024
	maxRuntimeErrorBytes = 1024
)

// ErrClosed is returned when Apply is called after Close.
var ErrClosed = errors.New("forward manager is closed")

// Snapshot is the configured rule merged with its current runtime state.
type Snapshot struct {
	config.Rule
	Running                 bool   `json:"running"`
	Error                   string `json:"error"`
	ActiveConnections       int64  `json:"active_connections"`
	BytesIn                 uint64 `json:"bytes_in"`
	BytesOut                uint64 `json:"bytes_out"`
	UDPSessions             int64  `json:"udp_sessions"`
	EffectiveMaxConnections int    `json:"effective_max_connections"`
}

// Manager owns a set of forwarding listeners. Apply operations are serialized.
// Apply rejects direct self-forwarding; cycles spanning multiple rules are not
// currently detected.
type Manager struct {
	applyMu sync.Mutex
	mu      sync.RWMutex
	closed  bool
	rules   []config.Rule
	runtime map[ruleKey]*runtime
}

type ruleKey string

type targetAddress struct {
	addr netip.Addr
	port uint16
}

func (a targetAddress) String() string {
	return netip.AddrPortFrom(a.addr, a.port).String()
}

type listenSpec struct {
	addr      netip.AddrPort
	canonical string
}

type compiledRule struct {
	rule       config.Rule
	key        ruleKey
	listen     listenSpec
	target     targetAddress
	allowed    []netip.Prefix
	maxConns   int
	udpTimeout time.Duration
}

type runtime struct {
	spec compiledRule

	tcp *tcpEndpoint
	udp *udpEndpoint

	activeConnections atomic.Int64
	bytesIn           atomic.Uint64
	bytesOut          atomic.Uint64
	udpSessions       atomic.Int64

	errMu     sync.RWMutex
	lastError string
}

// NewManager creates an empty forwarding manager.
func NewManager() *Manager {
	return &Manager{
		rules:   make([]config.Rule, 0),
		runtime: make(map[ruleKey]*runtime),
	}
}

// ValidateRules validates the complete rule set without resolving hostnames or
// opening sockets. Apply performs those environment-dependent checks.
func ValidateRules(rules []config.Rule) error {
	if len(rules) > maxRuleCount {
		return fmt.Errorf("too many rules: %d (maximum %d)", len(rules), maxRuleCount)
	}
	seenIDs := make(map[string]struct{}, len(rules))
	type checkedRule struct {
		rule   config.Rule
		listen listenSpec
	}
	checked := make([]checkedRule, 0, len(rules))

	for i, rule := range rules {
		prefix := fmt.Sprintf("rule %d", i+1)
		if len(rule.ID) > maxRuleIDBytes {
			return fmt.Errorf("%s: id exceeds %d bytes", prefix, maxRuleIDBytes)
		}
		if len(rule.Name) > maxRuleNameBytes {
			return fmt.Errorf("%s: name exceeds %d bytes", prefix, maxRuleNameBytes)
		}
		if len(rule.AllowedCIDRs) > maxAllowedCIDRs {
			return fmt.Errorf("%s: allowed_cidrs exceeds %d entries", prefix, maxAllowedCIDRs)
		}
		if rule.ID != "" {
			if _, exists := seenIDs[rule.ID]; exists {
				return fmt.Errorf("%s: duplicate id %q", prefix, rule.ID)
			}
			seenIDs[rule.ID] = struct{}{}
		}
		if !validNetwork(rule.Network) {
			return fmt.Errorf("%s: network must be tcp, udp, or both", prefix)
		}
		listen, err := parseListen(rule.Listen)
		if err != nil {
			return fmt.Errorf("%s: invalid listen address: %w", prefix, err)
		}
		if _, err := parseTargetSyntax(rule.Target); err != nil {
			return fmt.Errorf("%s: invalid target address: %w", prefix, err)
		}
		if _, err := parseAllowedCIDRs(rule.AllowedCIDRs); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		if rule.MaxConnections < 0 || rule.MaxConnections > config.MaxConnections {
			return fmt.Errorf("%s: max_connections must be between 0 and %d", prefix, config.MaxConnections)
		}
		if rule.UDPTimeout < 0 || rule.UDPTimeout > int(maxUDPTimeout/time.Second) {
			return fmt.Errorf("%s: udp_timeout must be between 0 and %d seconds", prefix, int(maxUDPTimeout/time.Second))
		}

		if rule.Enabled {
			for _, previous := range checked {
				if !sameTransport(previous.rule.Network, rule.Network, "tcp") &&
					!sameTransport(previous.rule.Network, rule.Network, "udp") {
					continue
				}
				if listenConflict(previous.listen, listen) {
					return fmt.Errorf("%s: listen address conflicts with enabled rule %q", prefix, previous.rule.ID)
				}
			}
			checked = append(checked, checkedRule{rule: rule, listen: listen})
		}
	}
	return nil
}

// Apply atomically replaces the rule set. It resolves enabled targets and
// opens every new listener before committing the new state. A bind failure
// rebuilds listeners for rules that were stopped during the transaction.
func (m *Manager) Apply(rules []config.Rule) error {
	if err := ValidateRules(rules); err != nil {
		return fmt.Errorf("validate rules: %w", err)
	}
	newRules := cloneRules(rules)

	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return ErrClosed
	}

	targets := make(map[ruleKey]targetAddress)
	for i, rule := range newRules {
		if !rule.Enabled {
			continue
		}
		key, err := keyForRule(rule, i)
		if err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), targetResolveTimeout)
		target, err := resolveTarget(ctx, rule.Target)
		cancel()
		if err != nil {
			return fmt.Errorf("rule %d: resolve target: %w", i+1, err)
		}
		targets[key] = target
	}

	desired := make([]compiledRule, 0, len(newRules))
	var localAddrs map[netip.Addr]struct{}
	localAddrsLoaded := false
	for i, rule := range newRules {
		key, err := keyForRule(rule, i)
		if err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
		listen, err := parseListen(rule.Listen)
		if err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
		allowed, err := parseAllowedCIDRs(rule.AllowedCIDRs)
		if err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
		spec := compiledRule{
			rule:       rule,
			key:        key,
			listen:     listen,
			target:     targets[key],
			allowed:    allowed,
			maxConns:   effectiveMaxConnections(rule),
			udpTimeout: effectiveUDPTimeout(rule),
		}
		if !rule.Enabled {
			continue
		}
		if spec.target.addr.IsValid() && !spec.target.addr.Is4() {
			return fmt.Errorf("rule %d: target %q resolved to a non-IPv4 address", i+1, rule.Target)
		}
		if canLoopBackToListener(spec.listen, spec.target) {
			if !localAddrsLoaded {
				var err error
				localAddrs, err = localAddressSet()
				if err != nil {
					return fmt.Errorf("rule %d: inspect local addresses: %w", i+1, err)
				}
				localAddrsLoaded = true
			}
			_, isLocal := localAddrs[spec.target.addr.Unmap().WithZone("")]
			if isLocal || spec.target.addr.IsLoopback() {
				return fmt.Errorf("rule %d: target %s would forward to its own listener", i+1, spec.target.String())
			}
		}
		desired = append(desired, spec)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}

	oldRuntime := m.runtime
	reused := make(map[ruleKey]*runtime, len(desired))
	toPrepare := make([]compiledRule, 0, len(desired))
	for _, spec := range desired {
		old, exists := oldRuntime[spec.key]
		if exists && old.spec.compatible(spec) {
			reused[spec.key] = old
			continue
		}
		toPrepare = append(toPrepare, spec)
	}

	toStop := make(map[ruleKey]*runtime)
	for key, old := range oldRuntime {
		if reused[key] == nil {
			toStop[key] = old
		}
	}

	for _, old := range toStop {
		old.close()
	}

	prepared := make(map[ruleKey]*runtime, len(toPrepare))
	for _, spec := range toPrepare {
		rt, err := prepareRuntime(spec)
		if err != nil {
			closeRuntimes(prepared)
			rollbackErr := m.restoreRuntimesLocked(oldRuntime, toStop)
			m.mu.Unlock()
			if rollbackErr != nil {
				return fmt.Errorf("apply rule %q: %w; rollback failed: %v", spec.rule.ID, err, rollbackErr)
			}
			return fmt.Errorf("apply rule %q: %w", spec.rule.ID, err)
		}
		prepared[spec.key] = rt
	}

	for _, rt := range prepared {
		rt.start()
	}
	newRulesByKey := make(map[ruleKey]config.Rule, len(newRules))
	for i, rule := range newRules {
		key, err := keyForRule(rule, i)
		if err == nil {
			newRulesByKey[key] = rule
		}
	}
	for key, rt := range reused {
		rt.spec.rule = cloneRule(newRulesByKey[key])
	}

	next := make(map[ruleKey]*runtime, len(reused)+len(prepared))
	for key, rt := range reused {
		next[key] = rt
	}
	for key, rt := range prepared {
		next[key] = rt
	}
	m.runtime = next
	m.rules = newRules
	m.mu.Unlock()
	return nil
}

func (m *Manager) restoreRuntimesLocked(oldRuntime map[ruleKey]*runtime, stopped map[ruleKey]*runtime) error {
	restored := make(map[ruleKey]*runtime, len(stopped))
	for key, old := range stopped {
		rt, err := prepareRuntime(old.spec)
		if err != nil {
			partial := make(map[ruleKey]*runtime)
			for oldKey, oldRT := range oldRuntime {
				if _, wasStopped := stopped[oldKey]; !wasStopped {
					partial[oldKey] = oldRT
				}
			}
			for restoredKey, restoredRT := range restored {
				restoredRT.start()
				partial[restoredKey] = restoredRT
			}
			for stoppedKey, stoppedRT := range stopped {
				if _, recovered := restored[stoppedKey]; !recovered {
					stoppedRT.setError(fmt.Errorf("listener rollback failed: %v", err))
					partial[stoppedKey] = stoppedRT
				}
			}
			m.runtime = partial
			return fmt.Errorf("restore rule %q: %w", old.spec.rule.ID, err)
		}
		rt.copyStatsFrom(old)
		restored[key] = rt
	}
	for _, rt := range restored {
		rt.start()
	}

	next := make(map[ruleKey]*runtime)
	for oldKey, oldRT := range oldRuntime {
		if _, wasStopped := stopped[oldKey]; !wasStopped {
			next[oldKey] = oldRT
		}
	}
	for key, rt := range restored {
		next[key] = rt
	}
	m.runtime = next
	return nil
}

// Snapshot returns the configured rules in input order with runtime counters.
func (m *Manager) Snapshot() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]Snapshot, 0, len(m.rules))
	for _, rule := range m.rules {
		key, _ := keyForRule(rule, len(result))
		snapshot := Snapshot{
			Rule:                    cloneRule(rule),
			EffectiveMaxConnections: effectiveMaxConnections(rule),
		}
		if rt := m.runtime[key]; rt != nil {
			rt.fillSnapshot(&snapshot)
		}
		result = append(result, snapshot)
	}
	return result
}

// Close stops every listener and active connection. It is safe to call more
// than once.
func (m *Manager) Close() {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	runtimes := m.runtime
	m.runtime = make(map[ruleKey]*runtime)
	m.mu.Unlock()

	for _, rt := range runtimes {
		rt.close()
	}
}

func prepareRuntime(spec compiledRule) (*runtime, error) {
	rt := &runtime{spec: spec}
	if hasTransport(spec.rule.Network, "tcp") {
		tcp, err := newTCPEndpoint(rt)
		if err != nil {
			return nil, err
		}
		rt.tcp = tcp
	}
	if hasTransport(spec.rule.Network, "udp") {
		udp, err := newUDPEndpoint(rt)
		if err != nil {
			if rt.tcp != nil {
				rt.tcp.close()
			}
			return nil, err
		}
		rt.udp = udp
	}
	return rt, nil
}

func (rt *runtime) start() {
	if rt.tcp != nil {
		rt.tcp.start()
	}
	if rt.udp != nil {
		rt.udp.start()
	}
}

func (rt *runtime) close() {
	if rt.tcp != nil {
		rt.tcp.close()
	}
	if rt.udp != nil {
		rt.udp.close()
	}
}

func (rt *runtime) running() bool {
	if rt.tcp != nil && !rt.tcp.running() {
		return false
	}
	if rt.udp != nil && !rt.udp.running() {
		return false
	}
	return rt.tcp != nil || rt.udp != nil
}

func (rt *runtime) fillSnapshot(snapshot *Snapshot) {
	snapshot.Running = rt.running()
	snapshot.ActiveConnections = rt.activeConnections.Load()
	snapshot.BytesIn = rt.bytesIn.Load()
	snapshot.BytesOut = rt.bytesOut.Load()
	snapshot.UDPSessions = rt.udpSessions.Load()
	rt.errMu.RLock()
	snapshot.Error = rt.lastError
	rt.errMu.RUnlock()
}

func (rt *runtime) setError(err error) {
	if err == nil {
		return
	}
	text := err.Error()
	if len(text) > maxRuntimeErrorBytes {
		text = text[:maxRuntimeErrorBytes]
	}
	rt.errMu.Lock()
	rt.lastError = text
	rt.errMu.Unlock()
}

func (rt *runtime) copyStatsFrom(old *runtime) {
	rt.bytesIn.Store(old.bytesIn.Load())
	rt.bytesOut.Store(old.bytesOut.Load())
	old.errMu.RLock()
	rt.lastError = old.lastError
	old.errMu.RUnlock()
}

func closeRuntimes(runtimes map[ruleKey]*runtime) {
	for _, rt := range runtimes {
		rt.close()
	}
}

func (spec compiledRule) compatible(next compiledRule) bool {
	if spec.rule.Enabled != next.rule.Enabled ||
		spec.rule.Network != next.rule.Network ||
		spec.listen.canonical != next.listen.canonical ||
		spec.target != next.target ||
		spec.maxConns != next.maxConns ||
		spec.udpTimeout != next.udpTimeout ||
		len(spec.allowed) != len(next.allowed) {
		return false
	}
	for i := range spec.allowed {
		if spec.allowed[i] != next.allowed[i] {
			return false
		}
	}
	return true
}

func resolveTarget(ctx context.Context, raw string) (targetAddress, error) {
	target, err := parseTargetSyntax(raw)
	if err != nil {
		return targetAddress{}, err
	}
	if target.addr.IsValid() {
		return targetAddress{addr: target.addr, port: target.port}, nil
	}

	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", target.hostname)
	if err != nil {
		return targetAddress{}, err
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.Is4() || !ip.IsValid() {
			continue
		}
		return targetAddress{addr: ip, port: target.port}, nil
	}
	return targetAddress{}, fmt.Errorf("hostname %q has no IPv4 address", target.hostname)
}

type parsedTarget struct {
	addr     netip.Addr
	hostname string
	port     uint16
}

func parseTargetSyntax(raw string) (parsedTarget, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return parsedTarget{}, err
	}
	if host == "" {
		return parsedTarget{}, errors.New("target host is empty")
	}
	port, err := parsePort(portText)
	if err != nil {
		return parsedTarget{}, err
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if !addr.Is4() {
			return parsedTarget{}, errors.New("target must resolve to IPv4")
		}
		if addr.IsUnspecified() {
			return parsedTarget{}, errors.New("target must not be the unspecified address")
		}
		return parsedTarget{addr: addr, port: port}, nil
	}
	if strings.ContainsAny(host, "/\\ \t\r\n") || strings.Contains(host, "%") {
		return parsedTarget{}, fmt.Errorf("invalid target hostname %q", host)
	}
	return parsedTarget{hostname: host, port: port}, nil
}

func parseListen(raw string) (listenSpec, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return listenSpec{}, err
	}
	port, err := parsePort(portText)
	if err != nil {
		return listenSpec{}, err
	}

	var addr netip.Addr
	if host == "" {
		addr = netip.IPv6Unspecified()
	} else {
		addr, err = netip.ParseAddr(host)
		if err != nil {
			return listenSpec{}, errors.New("listen host must be an IPv4 or IPv6 literal")
		}
		addr = addr.Unmap()
	}
	addrPort := netip.AddrPortFrom(addr, port)
	return listenSpec{
		addr:      addrPort,
		canonical: addrPort.String(),
	}, nil
}

func parsePort(text string) (uint16, error) {
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("port must be between 1 and 65535")
	}
	return uint16(port), nil
}

func parseAllowedCIDRs(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid allowed CIDR %q", value)
		}
		addr := prefix.Addr()
		if addr.Is4In6() {
			bits := prefix.Bits() - 96
			if bits < 0 {
				return nil, fmt.Errorf("invalid allowed CIDR %q", value)
			}
			prefix = netip.PrefixFrom(addr.Unmap(), bits).Masked()
		} else {
			prefix = prefix.Masked()
		}
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if comparison := prefixes[i].Addr().Compare(prefixes[j].Addr()); comparison != 0 {
			return comparison < 0
		}
		return prefixes[i].Bits() < prefixes[j].Bits()
	})
	unique := prefixes[:0]
	for _, prefix := range prefixes {
		if len(unique) == 0 || unique[len(unique)-1] != prefix {
			unique = append(unique, prefix)
		}
	}
	return unique, nil
}

func keyForRule(rule config.Rule, _ int) (ruleKey, error) {
	if rule.ID != "" {
		return ruleKey("id:" + rule.ID), nil
	}
	listen, err := parseListen(rule.Listen)
	if err != nil {
		return "", err
	}
	return ruleKey(fmt.Sprintf("anonymous:%s:%s:%s", rule.Network, listen.canonical, strings.ToLower(strings.TrimSpace(rule.Target)))), nil
}

func cloneRules(rules []config.Rule) []config.Rule {
	cloned := make([]config.Rule, len(rules))
	for i, rule := range rules {
		cloned[i] = cloneRule(rule)
	}
	return cloned
}

func cloneRule(rule config.Rule) config.Rule {
	rule.AllowedCIDRs = append([]string(nil), rule.AllowedCIDRs...)
	return rule
}

func effectiveMaxConnections(rule config.Rule) int {
	if rule.MaxConnections == 0 {
		return config.DefaultMaxConnections
	}
	return rule.MaxConnections
}

func effectiveUDPTimeout(rule config.Rule) time.Duration {
	if rule.UDPTimeout == 0 {
		return defaultUDPTimeout
	}
	return time.Duration(rule.UDPTimeout) * time.Second
}

func validNetwork(network string) bool {
	return network == "tcp" || network == "udp" || network == "both"
}

func hasTransport(network, transport string) bool {
	return network == "both" || network == transport
}

func sameTransport(left, right, transport string) bool {
	return hasTransport(left, transport) && hasTransport(right, transport)
}

func listenConflict(left, right listenSpec) bool {
	if left.addr.Port() != right.addr.Port() {
		return false
	}
	leftAddr := left.addr.Addr()
	rightAddr := right.addr.Addr()
	if leftAddr == rightAddr {
		return true
	}
	if leftAddr.IsUnspecified() {
		if leftAddr.Is6() {
			// Go's wildcard IPv6 listener is dual stack where the host supports it.
			return true
		}
		return rightAddr.Is4() || rightAddr.IsUnspecified()
	}
	if rightAddr.IsUnspecified() {
		if rightAddr.Is6() {
			return true
		}
		return leftAddr.Is4() || leftAddr.IsUnspecified()
	}
	return false
}

func canLoopBackToListener(listen listenSpec, target targetAddress) bool {
	if listen.addr.Port() != target.port {
		return false
	}
	listenAddr := listen.addr.Addr().Unmap().WithZone("")
	return listenAddr.IsUnspecified() || listenAddr == target.addr.Unmap().WithZone("")
}

func localAddressSet() (map[netip.Addr]struct{}, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	result := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		switch value := address.(type) {
		case *net.IPNet:
			if parsed, ok := netip.AddrFromSlice(value.IP); ok {
				result[parsed.Unmap().WithZone("")] = struct{}{}
			}
		case *net.IPAddr:
			if parsed, ok := netip.AddrFromSlice(value.IP); ok {
				result[parsed.Unmap().WithZone("")] = struct{}{}
			}
		}
	}
	return result, nil
}

func (rt *runtime) clientAllowed(addr netip.Addr) bool {
	if len(rt.spec.allowed) == 0 {
		return true
	}
	addr = addr.Unmap()
	addr = addr.WithZone("")
	for _, prefix := range rt.spec.allowed {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
