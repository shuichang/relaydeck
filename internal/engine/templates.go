package engine

import (
	"encoding/json"
)

// Template is a sing-box configuration starting point exposed by the API.
type Template struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
}

const templateSecurityNote = " 请替换本机端口、example.com/203.0.113.x 占位值与认证信息；配置等同管理员权限，只应使用可信 JSON。"

var builtinTemplates = buildTemplates()

// Templates returns fresh copies of the built-in sing-box 1.14.4 templates.
// Callers may edit a returned Config without changing the catalog.
func Templates() []Template {
	templates := make([]Template, len(builtinTemplates))
	for i, template := range builtinTemplates {
		templates[i] = Template{
			ID:          template.ID,
			Name:        template.Name,
			Description: template.Description,
			Config:      append(json.RawMessage(nil), template.Config...),
		}
	}
	return templates
}

// Templates is a compatibility wrapper for the package-level catalog.
func (m *Manager) Templates() []Template {
	return Templates()
}

func buildTemplates() []Template {
	return []Template{
		newTemplate(
			"direct-vless-reality",
			"VLESS + Reality",
			"本机 direct 入站经 VLESS/Reality 转发。替换服务器、UUID、SNI、Reality 公钥、short_id 与目标地址。",
			relayConfig("tcp", 12001, "203.0.113.20", 443, map[string]any{
				"type":        "vless",
				"tag":         "proxy",
				"server":      "203.0.113.10",
				"server_port": 443,
				"uuid":        "00000000-0000-4000-8000-000000000001",
				"flow":        "xtls-rprx-vision",
				"network":     "tcp",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "reality.example.com",
					"utls": map[string]any{
						"enabled":     true,
						"fingerprint": "chrome",
					},
					"reality": map[string]any{
						"enabled":    true,
						"public_key": "jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0",
						"short_id":   "01234567",
					},
				},
			}, nil),
		),
		newTemplate(
			"direct-hysteria2",
			"Hysteria2",
			"本机 direct 入站经 Hysteria2 转发。替换服务器、密码、TLS SNI 与目标地址。",
			relayConfig("", 12002, "203.0.113.20", 443, map[string]any{
				"type":        "hysteria2",
				"tag":         "proxy",
				"server":      "203.0.113.10",
				"server_port": 443,
				"password":    "replace-with-hysteria2-password",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "proxy.example.com",
					"alpn":        []string{"h3"},
				},
			}, nil),
		),
		newTemplate(
			"direct-tuic",
			"TUIC",
			"本机 direct 入站经 TUIC 转发。替换服务器、UUID、密码、TLS SNI 与目标地址。",
			relayConfig("", 12003, "203.0.113.20", 443, map[string]any{
				"type":               "tuic",
				"tag":                "proxy",
				"server":             "203.0.113.10",
				"server_port":        443,
				"uuid":               "00000000-0000-4000-8000-000000000001",
				"password":           "replace-with-tuic-password",
				"congestion_control": "bbr",
				"udp_relay_mode":     "native",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "proxy.example.com",
					"alpn":        []string{"h3"},
				},
			}, nil),
		),
		newTemplate(
			"direct-anytls",
			"AnyTLS",
			"本机 direct 入站经 AnyTLS 转发。替换服务器、密码、TLS SNI 与目标地址。",
			relayConfig("", 12004, "203.0.113.20", 443, map[string]any{
				"type":        "anytls",
				"tag":         "proxy",
				"server":      "203.0.113.10",
				"server_port": 443,
				"password":    "replace-with-anytls-password",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "proxy.example.com",
				},
			}, nil),
		),
		newTemplate(
			"direct-shadowsocks",
			"Shadowsocks 2022",
			"本机 direct 入站经 Shadowsocks 2022 转发。替换服务器、预共享密钥与目标地址。",
			relayConfig("", 12005, "203.0.113.20", 443, map[string]any{
				"type":        "shadowsocks",
				"tag":         "proxy",
				"server":      "203.0.113.10",
				"server_port": 8388,
				"method":      "2022-blake3-aes-128-gcm",
				"password":    "8JCsPssfgS8tiRwiMlhARg==",
			}, nil),
		),
		newTemplate(
			"direct-ssh",
			"SSH",
			"本机 direct 入站经 SSH 转发。替换服务器、用户、密码与目标地址；生产环境应填写可信 host_key。",
			relayConfig("tcp", 12006, "203.0.113.20", 22, map[string]any{
				"type":        "ssh",
				"tag":         "proxy",
				"server":      "203.0.113.10",
				"server_port": 22,
				"user":        "replace-user",
				"password":    "replace-with-ssh-password",
			}, nil),
		),
		newTemplate(
			"direct-wireguard",
			"WireGuard endpoint",
			"本机 direct 入站经 WireGuard endpoint 转发。替换接口地址、服务端、公私钥、allowed_ips 与目标地址。",
			relayConfig("", 12007, "203.0.113.20", 443, nil, []any{
				map[string]any{
					"type":        "wireguard",
					"tag":         "wg-ep",
					"address":     []string{"10.0.0.2/32"},
					"private_key": "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=",
					"peers": []any{
						map[string]any{
							"address":                       "203.0.113.10",
							"port":                          51820,
							"public_key":                    "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=",
							"allowed_ips":                   []string{"0.0.0.0/0", "::/0"},
							"persistent_keepalive_interval": 25,
						},
					},
				},
			}),
		),
		newTemplate(
			"direct-vless-ws-tls",
			"VLESS + WebSocket + TLS",
			"本机 direct 入站经 VLESS/WebSocket/TLS 转发。替换服务器、UUID、SNI、Host、WebSocket 路径与目标地址。",
			relayConfig("", 12008, "203.0.113.20", 443, map[string]any{
				"type":        "vless",
				"tag":         "proxy",
				"server":      "203.0.113.10",
				"server_port": 443,
				"uuid":        "00000000-0000-4000-8000-000000000001",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "ws.example.com",
					"alpn":        []string{"http/1.1"},
				},
				"transport": map[string]any{
					"type": "ws",
					"path": "/replace-websocket-path",
					"headers": map[string]any{
						"Host": "ws.example.com",
					},
				},
			}, nil),
		),
		newTemplate(
			"direct-vless-grpc-tls",
			"VLESS + gRPC + TLS",
			"本机 direct 入站经 VLESS/gRPC/TLS 转发。替换服务器、UUID、SNI、gRPC service_name 与目标地址。",
			relayConfig("", 12009, "203.0.113.20", 443, map[string]any{
				"type":        "vless",
				"tag":         "proxy",
				"server":      "203.0.113.10",
				"server_port": 443,
				"uuid":        "00000000-0000-4000-8000-000000000001",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": "grpc.example.com",
					"alpn":        []string{"h2"},
				},
				"transport": map[string]any{
					"type":         "grpc",
					"service_name": "ReplaceGrpcService",
				},
			}, nil),
		),
	}
}

func newTemplate(id, name, description string, config map[string]any) Template {
	raw, err := json.Marshal(config)
	if err != nil {
		panic("invalid built-in sing-box template: " + id + ": " + err.Error())
	}
	return Template{
		ID:          id,
		Name:        name,
		Description: description + templateSecurityNote,
		Config:      raw,
	}
}

func relayConfig(
	inboundNetwork string,
	listenPort int,
	target string,
	targetPort int,
	outbound map[string]any,
	endpoints []any,
) map[string]any {
	inbound := map[string]any{
		"type":        "direct",
		"tag":         "relay-in",
		"listen":      "127.0.0.1",
		"listen_port": listenPort,
	}
	if inboundNetwork != "" {
		inbound["network"] = inboundNetwork
	}

	outboundTag := "wg-ep"
	outbounds := []any{}
	if outbound != nil {
		outboundTag, _ = outbound["tag"].(string)
		outbounds = append(outbounds, outbound)
	}

	config := map[string]any{
		"$schema": "https://sing-box.sagernet.org/schema.json",
		"log": map[string]any{
			"level":     "info",
			"timestamp": true,
		},
		"inbounds":  []any{inbound},
		"outbounds": outbounds,
		"route": map[string]any{
			"rules": []any{
				map[string]any{
					"inbound":          []string{"relay-in"},
					"action":           "route",
					"outbound":         outboundTag,
					"override_address": target,
					"override_port":    targetPort,
				},
			},
			"final": outboundTag,
		},
	}
	if len(endpoints) > 0 {
		config["endpoints"] = endpoints
	}

	return config
}
