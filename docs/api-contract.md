# Internal implementation contract

Module: `github.com/shuichang/relaydeck`. Go 1.24+; avoid runtime dependencies where possible.

## UI API

All protected APIs accept a session cookie; JSON errors: `{"error":"message"}`.
Mutating requests require `X-RelayDeck-CSRF: 1` plus same-origin checks.

* `POST /api/login` body `{"username":"admin","password":"..."}` sets HttpOnly SameSite=Strict cookie.
* `POST /api/logout`
* `GET /api/status` -> `{"version":"...","uptime":123,"memory_bytes":123,"goroutines":12,"go_version":"...","hostname":"...","forward":{...},"engine":{...}}`.
* `GET /api/rules` -> array of rule objects merged with runtime fields `active_connections`, `bytes_in`, `bytes_out`, `effective_max_connections`, `error`, `running`, `udp_sessions`.
* `PUT /api/rules` -> `{"rules":[...]}` atomically replaces rules. Validate before replacing, rollback on bind failure. ID may be omitted for new rules.
* `POST /api/rules/probe` -> body `{"rule":{...}}`, response `{"checked_at":"...","target_address":"IPv4:port","listen":{"status":"listening|available|error","latency_ms":null,"message":"..."},"target":{"status":"reachable|error|unverified","latency_ms":0.5,"message":"...","udp_status":"unverified"}}`. Checks a draft without applying or persisting it. Four concurrent requests maximum; each has a four-second context. Temporary bind checks coordinate with rule replacement. `listening` only inspects the local listener and does not consume a forwarding slot. `available` means a draft port could be bound, not that it is serving. Target latency is an IPv4 TCP handshake excluding DNS; TCP probes send no payload and reset their connection on close. UDP targets are resolved but never marked reachable from a generic socket check. Results do not prove public firewall reachability, application authentication, or an inner tunnel destination.
* `GET /api/config/export` -> configuration JSON without credentials.
* `GET /api/engine` -> `{"enabled":false,"config":{},"available":false,"running":false,"version":"","error":"","logs":[]}`.
* `PUT /api/engine` -> `{"enabled":false,"config":{...}}`, validates via installed sing-box; persists only when validation and process start succeed.
* `GET /api/engine/templates` -> array of `{"id":"...","name":"...","description":"...","config":{...}}`. The first item is `direct-passthrough`, a TCP/UDP direct inbound-to-direct outbound template. It is an optional engine tunnel and does not limit native forwarding rules.
* `GET /healthz` public health without system information.

Rule shape:

```json
{"id":"...","name":"线路 A","enabled":true,"network":"both","listen":"[::]:443","target":"203.0.113.1:443","allowed_cidrs":[],"max_connections":0,"udp_timeout":60}
```

`network`: `tcp`, `udp`, `both`. Target is an IPv4 literal or hostname resolving to IPv4.
Listen `:port`, `0.0.0.0:port`, `[::]:port` (dual stack on Linux), specific IPv4 or IPv6.
`max_connections: 0` means the finite default `8192`; explicit values from `1` through `65536` are preserved. `effective_max_connections` reports the final per-rule limit.

## Backend package interfaces

`internal/forward` owns TCP/UDP lifecycle and tests. Native forwarding runs in the RelayDeck process and does not require the sing-box engine to be started. Expose:

```
func ValidateRules([]config.Rule) error
func NewManager() *Manager
func (m *Manager) Apply([]config.Rule) error
func (m *Manager) Snapshot() []Snapshot
func (m *Manager) Probe(context.Context, config.Rule) (ProbeResult, error)
func (m *Manager) Close()
```

`Snapshot` embeds `config.Rule` (flattened JSON), plus `Running bool`, `Error string`, `ActiveConnections int64`, `BytesIn uint64`, `BytesOut uint64`, `UDPSessions int64`, and `EffectiveMaxConnections int` with snake_case JSON tags.
Close old connections only for changed/removed rules. Preserve listeners and counters for unchanged rules.

The Reality template carries TCP and UDP allowed by its configuration; UDP additionally requires XUDP support at the remote end. It does not automatically convert other protocols into Reality. The SSH template remains TCP-only.

`web` owns static HTML/CSS/JS and `embed.go`, exposing `func FS() fs.FS`.

Installer uses `/usr/local/bin/relaydeck`, `/var/lib/relaydeck`, `/etc/relaydeck`,
unit `relaydeck.service`, command:
`relaydeck serve --data /var/lib/relaydeck --listen :9443 --tls-cert /etc/relaydeck/tls.crt --tls-key /etc/relaydeck/tls.key`.
Credentials stored only in `/var/lib/relaydeck/auth.json` (0600), initialize via:
`relaydeck init --data /var/lib/relaydeck --username admin --password-stdin`.
TLS generation via:
`relaydeck certificate --cert ... --key ... --hosts IP,localhost`.

`relaydeck version`, `relaydeck check --data ...`.
Optional sing-box binary `/usr/local/bin/sing-box`; package installer does not install it by default; `scripts/install-sing-box.sh` installs an explicit pinned version.
