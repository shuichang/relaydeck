# Internal implementation contract

Module: `github.com/shuichang/relaydeck`. Go 1.24+; avoid runtime dependencies where possible.

## UI API

All protected APIs accept a session cookie; JSON errors: `{"error":"message"}`.
Mutating requests require `X-RelayDeck-CSRF: 1` plus same-origin checks.

* `POST /api/login` body `{"username":"admin","password":"..."}` sets HttpOnly SameSite=Strict cookie.
* `POST /api/logout`
* `GET /api/status` -> `{"version":"...","uptime":123,"memory_bytes":123,"goroutines":12,"go_version":"...","hostname":"...","forward":{...},"engine":{...}}`.
* `GET /api/rules` -> array of rule objects merged with runtime fields `active_connections`, `bytes_in`, `bytes_out`, `effective_max_connections`, `error`, `error_network`, `last_error`, `last_error_at`, `last_error_network`, `last_recovery_at`, `running`, `udp_sessions`.
* `PUT /api/rules` -> `{"rules":[...]}` atomically replaces rules. Validate before replacing, rollback on bind failure. ID may be omitted for new rules.
* `POST /api/rules/probe` -> body `{"rule":{...}}`, response `{"checked_at":"...","target_address":"IPv4:port","listen":{"status":"listening|available|error","latency_ms":null,"message":"..."},"target":{"status":"reachable|error|unverified","latency_ms":0.5,"message":"...","udp_status":"unverified"}}`. Checks a draft without applying or persisting it. Four concurrent requests maximum; each has a four-second context. Temporary bind checks coordinate with rule replacement. `listening` only inspects the local listener and does not consume a forwarding slot. `available` means a draft port could be bound, not that it is serving. Target latency is an IPv4 TCP handshake excluding DNS; TCP probes send no payload and reset their connection on close. UDP targets are resolved but never marked reachable from a generic socket check. Results do not prove public firewall reachability, application authentication, or an inner tunnel destination.
* `GET /api/config/export` -> configuration JSON without credentials.
* `GET /api/engine` -> `{"enabled":false,"config":{},"available":false,"running":false,"version":"","error":"","logs":[]}`.
* `PUT /api/engine` -> `{"enabled":false,"config":{...}}`, validates via installed sing-box; persists only when validation and process start succeed.
* `GET /api/engine/templates` -> array of `{"id":"...","name":"...","description":"...","config":{...}}`. The first item is `direct-passthrough`, a TCP/UDP direct inbound-to-direct outbound template. It is an optional engine tunnel and does not limit native forwarding rules.
* `GET /api/settings/tls` -> public certificate metadata and source settings: `available`, `mode`, `certificate`, `cert_path`, `key_path`, `auto_reload_seconds`, `reload_error`, `updated_at`. `certificate` contains `subject`, `issuer`, `dns_names`, `ip_addresses`, `not_before`, `not_after`, `fingerprint_sha256`, `self_signed`, `valid_now`. No PEM/private-key contents are returned. Local HTTP development mode returns `available:false`.
* `PUT /api/settings/tls` -> one of `{"mode":"pem","certificate_pem":"...","private_key_pem":"..."}`, `{"mode":"files","cert_path":"/absolute/fullchain.pem","key_path":"/absolute/privkey.pem"}`, `{"mode":"self_signed","hosts":["panel.example.com","203.0.113.10"],"days":365}`, or `{"mode":"default"}`. Returns the same metadata as GET. Validates matching key, every chain block, provided issuer order, validity and server-auth usage; persists before atomically replacing the TLS handshake certificate. Invalid requests return 400 without changing the active certificate.
* `POST /api/settings/tls/reload` -> body `{}`, reloads the selected default/files source, returning the same metadata as GET. Bad/missing/expired source files retain the current certificate and return 400. File sources are also checked every 60 seconds. TLS mutations return 409 when TLS management is unavailable in local HTTP development mode.
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

`Snapshot` embeds `config.Rule` (flattened JSON), plus `Running bool`, `Error string`, `ErrorNetwork string`, `LastError string`, `LastErrorAt string`, `LastRecoveryAt string`, `LastErrorNetwork string`, `ActiveConnections int64`, `BytesIn uint64`, `BytesOut uint64`, `UDPSessions int64`, and `EffectiveMaxConnections int` with snake_case JSON tags.
`error` is the most recent unresolved error, preserved separately for TCP and UDP and also for listener faults; `error_network` identifies the protocol of that same unresolved error as `tcp`, `udp`, or empty when it is a rule-level error or `error` is empty. `last_error` is the most recent error text regardless of recovery; `last_error_at` and `last_recovery_at` are RFC3339 timestamps (empty before the first event), and `last_error_network` describes `last_error` as `tcp`, `udp`, or empty for a rule-level lifecycle error. The history network can differ from `error_network` after a newer error of another protocol has recovered. A successful TCP upstream connection clears only that TCP transport's recoverable errors. A UDP transport is marked recovered only after a real reply from its target; a successful UDP dial or write does not clear its error. Listener faults remain in `error` until the listener runtime is replaced. A recovered error clears from `error` while the `last_error*` history remains available to the UI.
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

`internal/paneltls` owns the immutable TLS certificate used by `http.Server.TLSConfig.GetCertificate`. Its independent `tls-settings.json` (0600) stores an atomic source/PEM snapshot, including the initial default certificate; it is intentionally excluded from `/api/config/export`. Source-file reloads validate before replacing the saved snapshot, so startup can retain the last known good pair during an incomplete renewal. Previously selected certificates which have since expired remain loadable for administrator repair, while new expired certificates are rejected. Restarting the HTTP service or forwarding listeners is unnecessary for rotation.
