# Internal implementation contract

Module: `github.com/shuichang/relaydeck`. Go 1.24+; avoid runtime dependencies where possible.

## UI API

All protected APIs accept a session cookie; JSON errors: `{"error":"message"}`.
Mutating requests require `X-RelayDeck-CSRF: 1` plus same-origin checks.

* `POST /api/login` body `{"username":"admin","password":"..."}` sets HttpOnly SameSite=Strict cookie.
* `POST /api/logout`
* `GET /api/status` -> `{"version":"...","uptime":123,"memory_bytes":123,"goroutines":12,"go_version":"...","hostname":"...","forward":{...},"engine":{...}}`.
* `GET /api/rules` -> array of rule objects merged with runtime fields `active_connections`, `bytes_in`, `bytes_out`, `error`, `running`, `udp_sessions`.
* `PUT /api/rules` -> `{"rules":[...]}` atomically replaces rules. Validate before replacing, rollback on bind failure. ID may be omitted for new rules.
* `GET /api/config/export` -> configuration JSON without credentials.
* `GET /api/engine` -> `{"enabled":false,"config":{},"available":false,"running":false,"version":"","error":"","logs":[]}`.
* `PUT /api/engine` -> `{"enabled":false,"config":{...}}`, validates via installed sing-box; persists only when validation and process start succeed.
* `GET /api/engine/templates` -> array of `{"id":"...","name":"...","description":"...","config":{...}}`.
* `GET /healthz` public health without system information.

Rule shape:

```json
{"id":"...","name":"线路 A","enabled":true,"network":"both","listen":"[::]:443","target":"203.0.113.1:443","allowed_cidrs":[],"max_connections":256,"udp_timeout":60}
```

`network`: `tcp`, `udp`, `both`. Target is an IPv4 literal or hostname resolving to IPv4.
Listen `:port`, `0.0.0.0:port`, `[::]:port` (dual stack on Linux), specific IPv4 or IPv6.

## Backend package interfaces

`internal/forward` owns TCP/UDP lifecycle and tests. Expose:

```
func ValidateRules([]config.Rule) error
func NewManager() *Manager
func (m *Manager) Apply([]config.Rule) error
func (m *Manager) Snapshot() []Snapshot
func (m *Manager) Close()
```

`Snapshot` embeds `config.Rule` (flattened JSON), plus `Running bool`, `Error string`, `ActiveConnections int64`, `BytesIn uint64`, `BytesOut uint64`, `UDPSessions int64` with snake_case JSON tags.
Close old connections only for changed/removed rules. Preserve listeners and counters for unchanged rules.

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
