#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly REPO="shuichang/relaydeck"
readonly BIN="/usr/local/bin/relaydeck"
readonly DATA_DIR="/var/lib/relaydeck"
readonly CONFIG_DIR="/etc/relaydeck"
readonly AUTH_FILE="${DATA_DIR}/auth.json"
readonly CERT_FILE="${CONFIG_DIR}/tls.crt"
readonly KEY_FILE="${CONFIG_DIR}/tls.key"
readonly UNIT="/etc/systemd/system/relaydeck.service"
readonly PREVIOUS="${DATA_DIR}/.relaydeck-previous"
readonly PREVIOUS_UNIT="${DATA_DIR}/.relaydeck-previous.service"

usage() {
  cat <<'EOF'
Usage:
  install.sh [--version vX.Y.Z]
  install.sh --uninstall

Environment:
  RELAYDECK_VERSION     Release tag, for example v0.1.1
  RELAYDECK_PUBLIC_IP   Public IP to put in the self-signed certificate

Re-running the installer performs an update and preserves auth/configuration.
Uninstall keeps /var/lib/relaydeck and /etc/relaydeck.
EOF
}

die() {
  printf 'relaydeck installer: %s\n' "$*" >&2
  exit 1
}

warn() {
  printf 'relaydeck installer: warning: %s\n' "$*" >&2
}

version="${RELAYDECK_VERSION:-}"
uninstall=0

while (($#)); do
  case "$1" in
    --version)
      (($# >= 2)) || die "--version requires a tag"
      version="$2"
      shift 2
      ;;
    --uninstall)
      uninstall=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ "${EUID}" -eq 0 ]] || die "run as root"
[[ "$(uname -s)" == "Linux" ]] || die "only Linux is supported"
command -v systemctl >/dev/null 2>&1 || die "systemctl is required"
[[ -d /run/systemd/system ]] || die "systemd is not the active service manager"

if ((uninstall)); then
  systemctl disable --now relaydeck.service >/dev/null 2>&1 || true
  rm -f -- "$UNIT" "$BIN"
  systemctl daemon-reload
  printf 'RelayDeck stopped and removed.\n'
  printf 'Preserved: %s %s\n' "$DATA_DIR" "$CONFIG_DIR"
  printf 'To reinstall, run this script again without --uninstall.\n'
  exit 0
fi

for cmd in curl tar sha256sum install od tr ss; do
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: $cmd"
done

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  armv7l|armv7) arch="armv7" ;;
  *) die "unsupported architecture: $(uname -m); supported: amd64, arm64, armv7" ;;
esac

if [[ -n "$version" ]]; then
  version="${version#v}"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] \
    || die "invalid release version: $version"
  tag="v${version}"
  release_base="https://github.com/${REPO}/releases/download/${tag}"
else
  release_base="https://github.com/${REPO}/releases/latest/download"
fi

install -d -m 0700 -o root -g root "$DATA_DIR" "$CONFIG_DIR"
tmpdir="$(mktemp -d "${DATA_DIR}/.install.XXXXXXXXXX")"
created_auth=0
installation_complete=0
staged_file=""
cleanup() {
  rm -rf -- "$tmpdir"
  if [[ -n "$staged_file" ]]; then
    rm -f -- "$staged_file"
  fi
  if ((created_auth == 1 && installation_complete == 0)); then
    rm -f -- "$AUTH_FILE"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

download() {
  local source="$1"
  local destination="$2"
  curl --fail --silent --show-error --location \
    --proto '=https' --tlsv1.2 \
    --retry 4 --retry-delay 2 --retry-all-errors \
    --output "$destination" "$source"
}

atomic_install() {
  local source_path="$1"
  local destination="$2"
  local mode="$3"
  local destination_dir destination_name
  destination_dir="$(dirname -- "$destination")"
  destination_name="$(basename -- "$destination")"
  staged_file="$(mktemp "${destination_dir}/.${destination_name}.XXXXXXXXXX")" || return 1
  install -m "$mode" -- "$source_path" "$staged_file" || return 1
  mv -f -- "$staged_file" "$destination" || return 1
  staged_file=""
}

# BEGIN HEALTH CHECK HELPERS
relaydeck_probe_pid=""

relaydeck_port_owned_by_pid() {
  local port="$1"
  local expected_pid="$2"
  local listeners line local_address process_info matched=0

  listeners="$(ss -H -ltnp 2>/dev/null)" || return 1
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    read -r _ _ _ local_address process_info <<< "$line"
    [[ "$local_address" == *":${port}" ]] || continue
    matched=1
    [[ "$process_info" == *"pid=${expected_pid},"* ]] || return 1
  done <<< "$listeners"
  ((matched == 1))
}

relaydeck_health_identity_matches() {
  local body="$1"
  local expected_version="$2"
  local actual_version

  [[ "$body" =~ \"status\"[[:space:]]*:[[:space:]]*\"ok\" ]] || return 1
  [[ "$body" =~ \"service\"[[:space:]]*:[[:space:]]*\"relaydeck\" ]] || return 1
  actual_version="$(
    sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<< "$body"
  )"
  [[ "$actual_version" == "$expected_version" ]]
}

# A Type=simple unit can report a successful start before its child exits on
# bind failure. Each probe therefore ties the listener and HTTP identity to a
# live, changing-safe MainPID. The caller requires several consecutive passes.
relaydeck_health_probe() {
  local unit="$1"
  local port="$2"
  local expected_version="$3"
  local main_pid health_file status body

  relaydeck_probe_pid=""
  systemctl is-active --quiet "$unit" || return 1
  main_pid="$(systemctl show --property=MainPID --value "$unit" 2>/dev/null)" || return 1
  [[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || return 1
  kill -0 "$main_pid" 2>/dev/null || return 1
  relaydeck_port_owned_by_pid "$port" "$main_pid" || return 1

  health_file="$(mktemp)" || return 1
  status="$(
    curl -k --silent --show-error --max-time 2 \
      --output "$health_file" \
      --write-out '%{http_code}' \
      "https://localhost:${port}/healthz"
  )" || {
    rm -f -- "$health_file"
    return 1
  }
  if [[ "$status" != "200" ]]; then
    rm -f -- "$health_file"
    return 1
  fi
  body="$(< "$health_file")"
  rm -f -- "$health_file"

  relaydeck_health_identity_matches "$body" "$expected_version" || return 1
  relaydeck_probe_pid="$main_pid"
}

wait_for_relaydeck_health() {
  local unit="$1"
  local port="$2"
  local expected_version="$3"
  local max_attempts="$4"
  local required_stable="$5"
  local stable_pid=""
  local consecutive=0
  local attempt

  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    # shellcheck disable=SC2310 # probe uses explicit returns and no set -e side effects
    if relaydeck_health_probe "$unit" "$port" "$expected_version"; then
      if [[ -z "$stable_pid" || "$relaydeck_probe_pid" != "$stable_pid" ]]; then
        stable_pid="$relaydeck_probe_pid"
        consecutive=1
      else
        consecutive=$((consecutive + 1))
      fi
    else
      consecutive=0
    fi
    ((consecutive >= required_stable)) && return 0
    sleep 1
  done
  return 1
}
# END HEALTH CHECK HELPERS

checksums="${tmpdir}/checksums.txt"
download "${release_base}/checksums.txt" "$checksums"
[[ -s "$checksums" ]] || die "release checksums.txt is empty"

if [[ -n "$version" ]]; then
  asset="relaydeck_${version}_linux_${arch}.tar.gz"
else
  candidate_count="$(
    awk -v suffix="_linux_${arch}.tar.gz" '
      $2 ~ "^relaydeck_[0-9A-Za-z._-]+" suffix "$" { print $2 }
    ' "$checksums" | sort -u | wc -l
  )"
  [[ "$candidate_count" -eq 1 ]] \
    || die "latest release must contain exactly one relaydeck asset for linux/${arch}"
  asset="$(
    awk -v suffix="_linux_${arch}.tar.gz" '
      $2 ~ "^relaydeck_[0-9A-Za-z._-]+" suffix "$" { print $2 }
    ' "$checksums" | sort -u
  )"
  [[ "$asset" =~ ^relaydeck_([0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?)_linux_${arch}\.tar\.gz$ ]] \
    || die "latest release asset name is not recognized: $asset"
  version="${BASH_REMATCH[1]}"
  tag="v${version}"
fi

matches="$(awk -v filename="$asset" '$2 == filename { count++ } END { print count + 0 }' "$checksums")"
[[ "$matches" -eq 1 ]] || die "checksums.txt must contain exactly one entry for ${asset}"

expected="$(
  awk -v filename="$asset" '$2 == filename { print tolower($1); exit }' "$checksums"
)"
[[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "invalid SHA-256 in checksums.txt for ${asset}"

archive="${tmpdir}/${asset}"
download "${release_base}/${asset}" "$archive"
actual="$(sha256sum "$archive" | awk '{ print tolower($1) }')"
[[ "$actual" == "$expected" ]] \
  || die "SHA-256 mismatch for ${asset}: expected ${expected}, got ${actual}"

tar -tzf "$archive" > "${tmpdir}/archive.list"
grep -Fxq 'relaydeck' "${tmpdir}/archive.list" \
  || die "release archive does not contain the expected relaydeck binary"
tar -xzOf "$archive" 'relaydeck' > "${tmpdir}/relaydeck"
[[ -s "${tmpdir}/relaydeck" ]] || die "release binary is empty"
chmod 0700 "${tmpdir}/relaydeck"

installed_version="$("${tmpdir}/relaydeck" version 2>&1)" \
  || die "downloaded binary cannot execute on this host"
read -r _ expected_health_version _ <<< "$installed_version"
[[ "$expected_health_version" == "v${version}" ]] \
  || die "downloaded binary version does not match ${tag}: ${installed_version}"

public_ip="${RELAYDECK_PUBLIC_IP:-}"

if [[ ! -e "$AUTH_FILE" ]]; then
  created_auth=1
else
  [[ -s "$AUTH_FILE" ]] || die "${AUTH_FILE} exists but is empty; refusing to reset credentials"
  chmod 0600 "$AUTH_FILE"
fi

if [[ -e "$CERT_FILE" || -e "$KEY_FILE" ]]; then
  [[ -s "$CERT_FILE" && -s "$KEY_FILE" ]] \
    || die "TLS certificate/key pair is incomplete; restore both files or remove neither safely"
  chmod 0600 "$CERT_FILE" "$KEY_FILE"
else
  if [[ -z "$public_ip" ]]; then
    public_ip="$(
      curl --fail --silent --show-error --location \
        --proto '=https' --tlsv1.2 --max-time 8 \
        'https://api64.ipify.org?format=text' 2>/dev/null || true
    )"
  fi
  [[ -n "$public_ip" ]] \
    || die "could not detect the public IP; rerun with RELAYDECK_PUBLIC_IP=<entry-ip>"
  [[ "$public_ip" =~ ^[0-9A-Fa-f:.]+$ && ${#public_ip} -le 64 ]] \
    || die "RELAYDECK_PUBLIC_IP is not a plain IP address"
  "${tmpdir}/relaydeck" certificate \
    --cert "$CERT_FILE" \
    --key "$KEY_FILE" \
    --hosts "${public_ip},localhost"
  chmod 0600 "$CERT_FILE" "$KEY_FILE"
fi

if ((created_auth)); then
  admin_password="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  [[ ${#admin_password} -eq 64 ]] || die "failed to generate a random administrator password"
  printf '%s\n' "$admin_password" \
    | "${tmpdir}/relaydeck" init \
        --data "$DATA_DIR" \
        --username admin \
        --password-stdin
  chmod 0600 "$AUTH_FILE"
fi

"${tmpdir}/relaydeck" check --data "$DATA_DIR"

unit_tmp="${tmpdir}/relaydeck.service"
cat > "$unit_tmp" <<'EOF'
[Unit]
Description=RelayDeck TCP/UDP relay panel
Documentation=https://github.com/shuichang/relaydeck
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=/var/lib/relaydeck
ExecStart=/usr/local/bin/relaydeck serve --data /var/lib/relaydeck --listen :9443 --tls-cert /etc/relaydeck/tls.crt --tls-key /etc/relaydeck/tls.key
Restart=on-failure
RestartSec=3s
TimeoutStopSec=20s
KillMode=mixed
UMask=0077
Environment="GOMEMLIMIT=96MiB"
Environment="GOGC=50"
LimitNOFILE=65535
LimitCORE=0
TasksMax=4096

NoNewPrivileges=true
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectClock=true
ProtectHostname=true
ProtectKernelLogs=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ProtectProc=invisible
ProcSubset=pid
InaccessiblePaths=/etc/shadow /etc/gshadow /etc/ssh /var/log
ReadOnlyPaths=/etc/relaydeck
ReadWritePaths=/var/lib/relaydeck
LockPersonality=true
MemoryDenyWriteExecute=true
RemoveIPC=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF

had_previous_bin=0
had_previous_unit=0
if [[ -x "$BIN" ]]; then
  atomic_install "$BIN" "$PREVIOUS" 0700
  had_previous_bin=1
fi
if [[ -f "$UNIT" ]]; then
  atomic_install "$UNIT" "$PREVIOUS_UNIT" 0600
  had_previous_unit=1
fi

restore_previous() {
  if ((had_previous_bin)); then
    atomic_install "$PREVIOUS" "$BIN" 0755 || true
  else
    rm -f -- "$BIN"
  fi
  if ((had_previous_unit)); then
    atomic_install "$PREVIOUS_UNIT" "$UNIT" 0644 || true
  else
    rm -f -- "$UNIT"
  fi
  systemctl daemon-reload || true
}

install_ok=1
atomic_install "${tmpdir}/relaydeck" "$BIN" 0755 || install_ok=0
if ((install_ok)); then
  atomic_install "$unit_tmp" "$UNIT" 0644 || install_ok=0
fi
if ((install_ok)); then
  systemctl daemon-reload || install_ok=0
fi
if ((install_ok)); then
  systemctl enable relaydeck.service >/dev/null || install_ok=0
fi
if ((install_ok)); then
  systemctl restart relaydeck.service || install_ok=0
fi

healthy=0
if ((install_ok)); then
  wait_for_relaydeck_health \
    relaydeck.service 9443 "$expected_health_version" 30 5 \
    && healthy=1
fi

if ((healthy == 0)); then
  systemctl status relaydeck.service --no-pager -l >&2 || true
  systemctl show --property=ActiveState --property=SubState --property=MainPID \
    relaydeck.service >&2 || true
  ss -H -ltnp 'sport = :9443' >&2 || true
  restore_previous
  if ((had_previous_bin && had_previous_unit)); then
    systemctl restart relaydeck.service || true
    warn "new version failed; restored the previous binary and systemd unit"
  else
    systemctl disable --now relaydeck.service >/dev/null 2>&1 || true
    warn "first installation failed; removed the partial service installation"
  fi
  exit 1
fi

installation_complete=1
management_host="${public_ip:-<server-ip>}"
if [[ "$management_host" == *:* ]]; then
  management_host="[${management_host}]"
fi
printf '\nRelayDeck %s is installed and healthy.\n' "$version"
printf 'Management: https://%s:9443/\n' "$management_host"
printf 'Certificate: %s\n' "$CERT_FILE"
printf 'The browser will show a self-signed certificate warning. Verify the IP and certificate fingerprint before adding a one-site exception.\n'
if ((created_auth)); then
  printf 'Username: admin\n'
  printf 'Password (shown once): %s\n' "$admin_password"
  unset admin_password
else
  printf 'Existing administrator credentials were preserved.\n'
fi
printf 'Data: %s\nConfig and TLS: %s\n' "$DATA_DIR" "$CONFIG_DIR"
