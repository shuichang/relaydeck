#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly VERSION="1.14.4"
readonly TARGET="/usr/local/bin/sing-box"

usage() {
  cat <<'EOF'
Usage:
  install-sing-box.sh [--force]

Installs the official sing-box 1.14.4 binary for linux/amd64, linux/arm64,
or linux/armv7 at /usr/local/bin/sing-box. Existing other versions are kept
unless --force is supplied.
EOF
}

die() {
  printf 'sing-box installer: %s\n' "$*" >&2
  exit 1
}

force=0
while (($#)); do
  case "$1" in
    --force) force=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ "${EUID}" -eq 0 ]] || die "run as root"
[[ "$(uname -s)" == "Linux" ]] || die "only Linux is supported"
for cmd in curl tar sha256sum install; do
  command -v "$cmd" >/dev/null 2>&1 || die "required command not found: $cmd"
done

case "$(uname -m)" in
  x86_64|amd64)
    arch="amd64"
    expected="699a1b510aa4396b2176d32d1181920399ebf1e267835da6f3b598d0f7c24968"
    ;;
  aarch64|arm64)
    arch="arm64"
    expected="436441f76ee2f7fe21b5b2d426d3cb261a480455a704865789ad533e4273ec26"
    ;;
  armv7l|armv7)
    arch="armv7"
    expected="fd164ed45ebc784f657a3e1d0e0ad0f68afdcff864ee57122c2cfed3ddb80a00"
    ;;
  *)
    die "unsupported architecture: $(uname -m); supported: amd64, arm64, armv7"
    ;;
esac

if [[ -e "$TARGET" || -L "$TARGET" ]]; then
  if [[ -x "$TARGET" ]]; then
    existing="$("$TARGET" version 2>/dev/null | sed -n '1p' || true)"
    case "$existing" in
      *"${VERSION}"*)
        printf 'sing-box %s is already installed at %s.\n' "$VERSION" "$TARGET"
        exit 0
        ;;
    esac
  fi
  ((force)) \
    || die "${TARGET} already exists with another version; rerun with --force to replace it"
fi

tmpdir="$(mktemp -d)"
stage="$(mktemp /usr/local/bin/.sing-box.XXXXXXXXXX)"
cleanup() {
  rm -rf -- "$tmpdir"
  rm -f -- "$stage"
}
trap cleanup EXIT

asset="sing-box-${VERSION}-linux-${arch}.tar.gz"
url="https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${asset}"
archive="${tmpdir}/${asset}"

curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.2 \
  --retry 4 --retry-delay 2 --retry-all-errors \
  --output "$archive" "$url"

actual="$(sha256sum "$archive" | awk '{ print tolower($1) }')"
[[ "$actual" == "$expected" ]] \
  || die "SHA-256 mismatch for ${asset}: expected ${expected}, got ${actual}"

member="$(
  tar -tzf "$archive" |
    awk -v name="sing-box-${VERSION}-linux-${arch}/sing-box" '
      $0 == name && !found { found = $0 }
      END { print found }
    '
)"
[[ "$member" == "sing-box-${VERSION}-linux-${arch}/sing-box" ]] \
  || die "release archive does not contain the expected sing-box binary"
tar -xzOf "$archive" "$member" > "$stage"
[[ -s "$stage" ]] || die "release binary is empty"
chmod 0700 "$stage"

actual_version="$("$stage" version 2>&1)" \
  || die "downloaded sing-box binary cannot execute on this host"
case "$actual_version" in
  *"${VERSION}"*) ;;
  *) die "downloaded sing-box version does not match ${VERSION}: ${actual_version}" ;;
esac

if [[ -e "$TARGET" || -L "$TARGET" ]]; then
  install -m 0700 "$TARGET" "${TARGET}.relaydeck-backup"
  printf 'Previous binary saved as %s\n' "${TARGET}.relaydeck-backup"
fi
chmod 0755 "$stage"
mv -f -- "$stage" "$TARGET"

printf 'Installed sing-box %s at %s.\n' "$VERSION" "$TARGET"
printf 'No sing-box service or configuration was changed. Configure the engine in RelayDeck and validate it there.\n'
