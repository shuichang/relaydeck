package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestTemplateCatalogKeepsLegacyIDsAndAddsDirectPassthroughFirst(t *testing.T) {
	templates := Templates()
	wantIDs := []string{
		"direct-passthrough",
		"direct-vless-reality",
		"direct-hysteria2",
		"direct-tuic",
		"direct-anytls",
		"direct-shadowsocks",
		"direct-ssh",
		"direct-wireguard",
		"direct-vless-ws-tls",
		"direct-vless-grpc-tls",
	}
	gotIDs := make([]string, len(templates))
	for i, template := range templates {
		gotIDs[i] = template.ID
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("template IDs = %v, want %v", gotIDs, wantIDs)
	}

	passthrough := templates[0]
	for _, phrase := range []string{"可选", "原生", "无需启动 sing-box"} {
		if !strings.Contains(passthrough.Description, phrase) {
			t.Errorf("direct-passthrough description lacks %q: %s", phrase, passthrough.Description)
		}
	}
	config := decodeTemplateConfig(t, passthrough)
	inbound := firstTemplateObject(t, config, "inbounds")
	if inbound["type"] != "direct" {
		t.Fatalf("direct-passthrough inbound type = %v, want direct", inbound["type"])
	}
	if inbound["network"] != nil {
		t.Fatalf("direct-passthrough unexpectedly limits TCP/UDP inbound with network=%v", inbound["network"])
	}
	outbound := firstTemplateObject(t, config, "outbounds")
	if outbound["type"] != "direct" || outbound["tag"] != "direct" {
		t.Fatalf("direct-passthrough outbound = %+v, want direct/direct", outbound)
	}
	rule := firstTemplateObject(t, templateObject(t, config, "route"), "rules")
	if rule["action"] != "route" || rule["inbound"] == nil || rule["outbound"] != "direct" {
		t.Fatalf("direct-passthrough route uses unexpected shape: %+v", rule)
	}
}

func TestRealityTemplateCarriesConfiguredTCPAndUDPWithoutAutomaticConversion(t *testing.T) {
	template := findTemplate(t, Templates(), "direct-vless-reality")
	for _, phrase := range []string{"TCP", "UDP", "XUDP", "不会", "自动转换"} {
		if !strings.Contains(template.Description, phrase) {
			t.Errorf("Reality description lacks %q: %s", phrase, template.Description)
		}
	}

	config := decodeTemplateConfig(t, template)
	inbound := firstTemplateObject(t, config, "inbounds")
	if inbound["type"] != "direct" {
		t.Fatalf("Reality inbound type = %v, want direct", inbound["type"])
	}
	if inbound["network"] != nil {
		t.Fatalf("Reality inbound must carry both TCP and UDP, got network=%v", inbound["network"])
	}

	outbound := firstTemplateObject(t, config, "outbounds")
	if outbound["type"] != "vless" || outbound["tag"] != "proxy" {
		t.Fatalf("Reality outbound = %+v, want vless/proxy", outbound)
	}
	if outbound["network"] != nil {
		t.Fatalf("Reality outbound must not restrict proxied traffic with network=%v", outbound["network"])
	}
	if outbound["packet_encoding"] != "xudp" {
		t.Fatalf("Reality packet_encoding = %v, want xudp", outbound["packet_encoding"])
	}

	rule := firstTemplateObject(t, templateObject(t, config, "route"), "rules")
	if rule["action"] != "route" || rule["outbound"] != "proxy" {
		t.Fatalf("Reality route uses unexpected 1.14.4 syntax: %+v", rule)
	}
}

func TestSSHTemplateRemainsTCPOnly(t *testing.T) {
	template := findTemplate(t, Templates(), "direct-ssh")
	if !strings.Contains(template.Description, "TCP") {
		t.Errorf("SSH description should state its TCP-only boundary: %s", template.Description)
	}
	config := decodeTemplateConfig(t, template)
	inbound := firstTemplateObject(t, config, "inbounds")
	if inbound["type"] != "direct" || inbound["network"] != "tcp" {
		t.Fatalf("SSH inbound = %+v, want direct TCP-only", inbound)
	}
	outbound := firstTemplateObject(t, config, "outbounds")
	if outbound["type"] != "ssh" {
		t.Fatalf("SSH outbound type = %v, want ssh", outbound["type"])
	}
}

func findTemplate(t *testing.T, templates []Template, id string) Template {
	t.Helper()
	for _, template := range templates {
		if template.ID == id {
			return template
		}
	}
	t.Fatalf("template %q not found", id)
	return Template{}
}

func decodeTemplateConfig(t *testing.T, template Template) map[string]any {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal(template.Config, &config); err != nil {
		t.Fatalf("decode template %q: %v", template.ID, err)
	}
	return config
}

func templateObject(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	object, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want object", key, parent[key])
	}
	return object
}

func firstTemplateObject(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	values, ok := parent[key].([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("%q is %T with length unavailable, want non-empty array", key, parent[key])
	}
	object, ok := values[0].(map[string]any)
	if !ok {
		t.Fatalf("%s[0] is %T, want object", key, values[0])
	}
	return object
}
