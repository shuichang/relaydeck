package forward

import (
	"testing"

	"github.com/shuichang/relaydeck/internal/config"
)

func TestExplicitIPv4WildcardUsesIPv4Only(t *testing.T) {
	v4, err := parseListen("0.0.0.0:12345")
	if err != nil {
		t.Fatal(err)
	}
	v6, err := parseListen("[::]:12345")
	if err != nil {
		t.Fatal(err)
	}
	if listenNetwork(v4, "tcp") != "tcp4" || listenNetwork(v4, "udp") != "udp4" {
		t.Fatal("an explicit IPv4 wildcard must not unexpectedly accept IPv6")
	}
	if listenNetwork(v6, "tcp") != "tcp" {
		t.Fatal("IPv6 wildcard must retain dual-stack support")
	}
}

func TestUDPTimeoutCannotOverflowDuration(t *testing.T) {
	rule := config.Rule{ID: "huge-timeout", Network: "udp", Listen: "127.0.0.1:12345", Target: "192.0.2.1:443", UDPTimeout: int(^uint(0) >> 1)}
	if err := ValidateRules([]config.Rule{rule}); err == nil {
		t.Fatal("huge timeout can overflow time.Duration and bypass validation")
	}
}
