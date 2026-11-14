#!/usr/bin/env bash
set -euo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)" || exit 1
installer="${script_dir}/install.sh"
tmpdir="$(mktemp -d)"
checks=0

cleanup() {
  rm -rf -- "$tmpdir"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

pass() {
  checks=$((checks + 1))
  printf 'PASS: %s\n' "$*"
}

for cmd in sed mktemp; do
  command -v "$cmd" >/dev/null 2>&1 || fail "required test command not found: $cmd"
done

sed -n '/^# BEGIN PANEL PORT HELPERS$/,/^# END PANEL PORT HELPERS$/p' \
  "$installer" > "${tmpdir}/port-helpers.sh"
[[ -s "${tmpdir}/port-helpers.sh" ]] || fail "panel port helper block was not found"
# shellcheck source=/dev/null
source "${tmpdir}/port-helpers.sh"

for value in 1 443 9443 18443 65535; do
  relaydeck_validate_panel_port "$value" \
    || fail "valid panel port was rejected: $value"
done
pass "valid panel ports are accepted"

for value in "" 0 65536 100000 09443 abc ':9443' '[::]:9443'; do
  if relaydeck_validate_panel_port "$value"; then
    fail "invalid panel port was accepted: $value"
  fi
done
pass "invalid panel ports are rejected"

cat > "${tmpdir}/relaydeck.service" <<'EOF'
[Service]
ExecStart=/usr/local/bin/relaydeck serve --data /var/lib/relaydeck --listen :18443 --tls-cert /etc/relaydeck/tls.crt --tls-key /etc/relaydeck/tls.key
EOF
[[ "$(relaydeck_panel_port_from_service "${tmpdir}/relaydeck.service")" == "18443" ]] \
  || fail "existing unit port was not detected"
pass "existing unit port is preserved"

cat > "${tmpdir}/relaydeck.init" <<'EOF'
#!/sbin/openrc-run
command_args="serve --data /var/lib/relaydeck --listen :28543 --tls-cert /etc/relaydeck/tls.crt --tls-key /etc/relaydeck/tls.key"
EOF
[[ "$(relaydeck_panel_port_from_service "${tmpdir}/relaydeck.init")" == "28543" ]] \
  || fail "existing OpenRC service port was not detected"
pass "existing OpenRC service port is preserved"

cat > "${tmpdir}/invalid.service" <<'EOF'
[Service]
ExecStart=/usr/local/bin/relaydeck serve --data /var/lib/relaydeck --listen [::]:0 --tls-cert /etc/relaydeck/tls.crt --tls-key /etc/relaydeck/tls.key
EOF
if relaydeck_panel_port_from_service "${tmpdir}/invalid.service"; then
  fail "invalid existing unit port was accepted"
fi
pass "invalid existing unit port is ignored"

printf 'All panel port installer tests passed (%s checks).\n' "$checks"
