#!/usr/bin/env bash
set -euo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)" || exit 1
installer="${script_dir}/install.sh"
tmpdir="$(mktemp -d)"
mock_log="${tmpdir}/mock-calls.log"
mock_violations="${tmpdir}/mock-violations.log"
detect_stdout="${tmpdir}/detect.stdout"
detect_stderr="${tmpdir}/detect.stderr"
detect_status=0
detect_output=""
checks=0

cleanup() {
  rm -rf -- "$tmpdir"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  printf 'DIAG: detect_status=%s detect_output=%q\n' \
    "$detect_status" "$detect_output" >&2
  if [[ -s "${mock_violations}" ]]; then
    printf 'DIAG: mock contract violations:\n' >&2
    sed -n '1,20p' "${mock_violations}" >&2
  fi
  if [[ -s "${mock_log}" ]]; then
    printf 'DIAG: mocked calls:\n' >&2
    sed -n '1,30p' "${mock_log}" >&2
  fi
  exit 1
}

pass() {
  checks=$((checks + 1))
  printf 'PASS: %s\n' "$*"
}

for cmd in awk sed mktemp; do
  command -v "$cmd" >/dev/null 2>&1 || fail "required test command not found: $cmd"
done

sed -n '/^# BEGIN PUBLIC IP HELPERS$/,/^# END PUBLIC IP HELPERS$/p' \
  "$installer" > "${tmpdir}/public-ip-helpers.sh"
[[ -s "${tmpdir}/public-ip-helpers.sh" ]] \
  || fail "public IP helper block was not found in install.sh"
# shellcheck source=/dev/null
source "${tmpdir}/public-ip-helpers.sh"

for function_name in \
  relaydeck_is_ipv4 \
  relaydeck_is_ipv6 \
  relaydeck_is_public_ipv4 \
  relaydeck_is_public_ipv6 \
  relaydeck_detect_public_ip; do
  declare -F "$function_name" >/dev/null \
    || fail "required public IP helper is missing: ${function_name}"
done

mock_record() {
  local command_name="$1"
  shift
  {
    printf '%s' "$command_name"
    printf ' %q' "$@"
    printf '\n'
  } >> "${mock_log}"
}

mock_violation() {
  printf '%s\n' "$*" >> "${mock_violations}"
}

reset_mocks() {
  : > "${mock_log}"
  : > "${mock_violations}"
  rm -f -- "${tmpdir}/curl4.count" "${tmpdir}/curl6.count"
  rm -f -- "${tmpdir}"/ip4-route "${tmpdir}"/ip4-addr
  rm -f -- "${tmpdir}"/ip6-route "${tmpdir}"/ip6-addr
  MOCK_IP4_ROUTE_STATUS=1
  MOCK_IP4_ADDR_STATUS=1
  MOCK_IP6_ROUTE_STATUS=1
  MOCK_IP6_ADDR_STATUS=1

  local family index
  for family in 4 6; do
    for index in 1 2 3 4 5; do
      printf -v "MOCK_CURL${family}_STATUS_${index}" '%s' 7
      printf -v "MOCK_CURL${family}_BODY_${index}" '%s' ''
    done
  done
}

set_ip_mock() {
  local family="$1"
  local source="$2"
  local body="$3"
  local status="${4:-0}"
  local output_file

  case "${family}:${source}" in
    4:route)
      output_file="${tmpdir}/ip4-route"
      MOCK_IP4_ROUTE_STATUS="$status"
      ;;
    4:addr)
      output_file="${tmpdir}/ip4-addr"
      MOCK_IP4_ADDR_STATUS="$status"
      ;;
    6:route)
      output_file="${tmpdir}/ip6-route"
      MOCK_IP6_ROUTE_STATUS="$status"
      ;;
    6:addr)
      output_file="${tmpdir}/ip6-addr"
      MOCK_IP6_ADDR_STATUS="$status"
      ;;
    *)
      fail "invalid ip mock specification: ${family}:${source}"
      ;;
  esac
  printf '%s\n' "$body" > "$output_file"
}

set_curl_mock() {
  local family="$1"
  local index="$2"
  local status="$3"
  local body="$4"
  printf -v "MOCK_CURL${family}_STATUS_${index}" '%s' "$status"
  printf -v "MOCK_CURL${family}_BODY_${index}" '%s' "$body"
}

ip() {
  mock_record ip "$@"
  case "$*" in
    "-4 route get 1.1.1.1")
      cat -- "${tmpdir}/ip4-route" 2>/dev/null || true
      return "$MOCK_IP4_ROUTE_STATUS"
      ;;
    "-o -4 addr show up scope global")
      cat -- "${tmpdir}/ip4-addr" 2>/dev/null || true
      return "$MOCK_IP4_ADDR_STATUS"
      ;;
    "-6 route get 2606:4700:4700::1111")
      cat -- "${tmpdir}/ip6-route" 2>/dev/null || true
      return "$MOCK_IP6_ROUTE_STATUS"
      ;;
    "-o -6 addr show up scope global")
      cat -- "${tmpdir}/ip6-addr" 2>/dev/null || true
      return "$MOCK_IP6_ADDR_STATUS"
      ;;
    *)
      mock_violation "unexpected ip invocation: $*"
      return 1
      ;;
  esac
}

curl() {
  mock_record curl "$@"

  local family=""
  local argument previous=""
  local has_connect_timeout=0 has_max_time=0 has_noproxy=0 has_https=0
  for argument in "$@"; do
    case "$argument" in
      -4)
        [[ -z "$family" ]] || mock_violation "curl used multiple IP families: $*"
        family=4
        ;;
      -6)
        [[ -z "$family" ]] || mock_violation "curl used multiple IP families: $*"
        family=6
        ;;
      --connect-timeout=2|--max-time=4|--noproxy='*')
        case "$argument" in
          --connect-timeout=2) has_connect_timeout=1 ;;
          --max-time=4) has_max_time=1 ;;
          --noproxy='*') has_noproxy=1 ;;
        esac
        ;;
      --connect-timeout|--max-time|--noproxy)
        previous="$argument"
        ;;
      2)
        [[ "$previous" == "--connect-timeout" ]] && has_connect_timeout=1
        previous=""
        ;;
      4)
        [[ "$previous" == "--max-time" ]] && has_max_time=1
        previous=""
        ;;
      '*')
        [[ "$previous" == "--noproxy" ]] && has_noproxy=1
        previous=""
        ;;
      https://*)
        has_https=1
        ;;
      http://*)
        mock_violation "curl attempted to use plain HTTP: $*"
        ;;
    esac
  done

  [[ "$family" == "4" || "$family" == "6" ]] \
    || mock_violation "curl did not select IPv4 or IPv6 explicitly: $*"
  [[ "$has_connect_timeout" -eq 1 ]] \
    || mock_violation "curl did not use --connect-timeout 2: $*"
  [[ "$has_max_time" -eq 1 ]] \
    || mock_violation "curl did not use --max-time 4: $*"
  [[ "$has_noproxy" -eq 1 ]] \
    || mock_violation "curl did not disable proxies with --noproxy '*': $*"
  [[ "$has_https" -eq 1 ]] \
    || mock_violation "curl did not request an HTTPS URL: $*"

  [[ "$family" == "4" || "$family" == "6" ]] || return 7

  local count_file="${tmpdir}/curl${family}.count"
  local count=0
  if [[ -f "$count_file" ]]; then
    IFS= read -r count < "$count_file" || true
  fi
  count=$((count + 1))
  printf '%s\n' "$count" > "$count_file"

  local status_name="MOCK_CURL${family}_STATUS_${count}"
  local body_name="MOCK_CURL${family}_BODY_${count}"
  local status="${!status_name-7}"
  local body="${!body_name-}"
  printf '%s' "$body"
  return "$status"
}

run_detect() {
  : > "${detect_stdout}"
  : > "${detect_stderr}"
  set +e
  relaydeck_detect_public_ip "$@" > "${detect_stdout}" 2> "${detect_stderr}"
  detect_status=$?
  set -e
  detect_output="$(< "${detect_stdout}")"
}

mock_call_count() {
  local prefix="$1"
  local line
  local count=0
  if [[ -f "${mock_log}" ]]; then
    while IFS= read -r line; do
      if [[ "$line" == "$prefix"* ]]; then
        count=$((count + 1))
      fi
    done < "${mock_log}"
  fi
  printf '%s\n' "$count"
}

assert_mock_contract() {
  [[ ! -s "${mock_violations}" ]] || fail "mock call contract was violated"
}

assert_detect_ok() {
  local expected="$1"
  local label="$2"
  local line_count
  [[ "$detect_status" -eq 0 ]] \
    || fail "${label}: expected status 0, got ${detect_status}"
  [[ "$detect_output" == "$expected" ]] \
    || fail "${label}: expected ${expected@Q}, got ${detect_output@Q}"
  line_count="$(awk 'END { print NR }' "${detect_stdout}")"
  [[ "$line_count" -le 1 ]] \
    || fail "${label}: detection wrote more than one line"
  assert_mock_contract
  pass "$label"
}

assert_detect_failure() {
  local expected_status="$1"
  local label="$2"
  [[ "$detect_status" -eq "$expected_status" ]] \
    || fail "${label}: expected status ${expected_status}, got ${detect_status}"
  [[ -z "$detect_output" ]] \
    || fail "${label}: failure wrote an address to stdout"
  assert_mock_contract
  pass "$label"
}

assert_call_count() {
  local prefix="$1"
  local expected="$2"
  local label="$3"
  local actual
  actual="$(mock_call_count "$prefix")"
  [[ "$actual" -eq "$expected" ]] \
    || fail "${label}: expected ${expected} ${prefix} calls, got ${actual}"
}

assert_call_at_least() {
  local prefix="$1"
  local minimum="$2"
  local label="$3"
  local actual
  actual="$(mock_call_count "$prefix")"
  [[ "$actual" -ge "$minimum" ]] \
    || fail "${label}: expected at least ${minimum} ${prefix} calls, got ${actual}"
}

assert_predicate() {
  local function_name="$1"
  local value="$2"
  local expected="$3"
  local label="$4"
  if "$function_name" "$value"; then
    [[ "$expected" == "true" ]] \
      || fail "${label}: ${function_name} accepted ${value@Q}"
  else
    [[ "$expected" == "false" ]] \
      || fail "${label}: ${function_name} rejected ${value@Q}"
  fi
  pass "${label}: ${function_name} ${value@Q} -> ${expected}"
}

for value in \
  '1.2.3.4' '10.0.0.1' '127.0.0.1' '203.0.113.10' '255.255.255.255'; do
  assert_predicate relaydeck_is_ipv4 "$value" true "IPv4 syntax"
done
for value in \
  '999.1.1.1' '01.2.3.4' '1.2.3' '1.2.3.4.5' '1.2.3.256' \
  ' 1.2.3.4' $'1.2.3.4\n' 'example.com' '<html>1.2.3.4</html>'; do
  assert_predicate relaydeck_is_ipv4 "$value" false "IPv4 syntax"
done

for value in \
  '2606:4700:4700::1111' '2001:db8::1' 'fe80::1' '::' '::1' \
  '1:2:3:4:5:6:7:8' '2001:db8:0:0:0:0:2:1' '2001:db8::2:1' \
  '::ffff:192.0.2.128' '2001:db8::192.0.2.1'; do
  assert_predicate relaydeck_is_ipv6 "$value" true "IPv6 syntax"
done
for value in \
  '2001:db8:::1' '2001:db8::1::2' '1:2:3:4:5:6:7' \
  '1:2:3:4:5:6:7:8:9' '12345::1' '2001:db8::g' \
  ':2001::1' '2001::1:' '2001:db8:1:2:3:4:5:6::' \
  '::ffff:999.1.1.1' '::ffff:192.0.02.1' 'example.com' \
  '<html>::1</html>' $'2606:4700:4700::1111\n2001:4860:4860::8888'; do
  assert_predicate relaydeck_is_ipv6 "$value" false "IPv6 syntax"
done

for value in '1.1.1.1' '8.8.8.8' '93.184.216.34'; do
  assert_predicate relaydeck_is_public_ipv4 "$value" true "public IPv4"
done
for value in \
  '0.0.0.0' '10.0.0.1' '100.64.0.1' '127.0.0.1' '169.254.1.1' \
  '172.16.0.1' '192.168.1.1' '192.0.2.1' '198.51.100.1' \
  '203.0.113.1' '224.0.0.1' '240.0.0.1' '255.255.255.255'; do
  assert_predicate relaydeck_is_public_ipv4 "$value" false "public IPv4"
done

for value in '2606:4700:4700::1111' '2001:4860:4860::8888' '3fff:1000::1'; do
  assert_predicate relaydeck_is_public_ipv6 "$value" true "public IPv6"
done
for value in \
  '::' '::1' '2001:db8::1' '2001:DB8:0:1::1' '3fff::1' \
  'fc00::1' 'fd12:3456::1' 'fe80::1' 'ff02::1' '::ffff:8.8.8.8'; do
  assert_predicate relaydeck_is_public_ipv6 "$value" false "public IPv6"
done

reset_mocks
set_ip_mock 4 route \
  '1.1.1.1 via 192.168.1.1 dev eth0 src 8.8.8.8 uid 0'
set_ip_mock 4 addr \
  '2: eth0    inet 8.8.8.8/24 brd 8.8.8.255 scope global eth0\       valid_lft forever preferred_lft forever'
run_detect
assert_detect_ok '8.8.8.8' 'public local IPv4 wins when external probing is unavailable'
assert_call_count 'curl ' 0 'local IPv4 success skips external probes'

reset_mocks
set_ip_mock 4 route \
  '1.1.1.1 via 192.168.1.1 dev eth0 src 10.0.0.7 uid 0'
set_ip_mock 4 addr \
  '2: eth0    inet 10.0.0.7/24 brd 10.0.0.255 scope global eth0\       valid_lft forever preferred_lft forever'
set_ip_mock 6 route \
  '2606:4700:4700::1111 from :: via fe80::1 dev eth0 src 2606:4700:4700::1234 metric 1024 pref medium'
set_ip_mock 6 addr \
  '2: eth0    inet6 2606:4700:4700::1234/64 scope global\       valid_lft forever preferred_lft forever'
run_detect
assert_detect_ok '2606:4700:4700::1234' \
  'public local IPv6 is used when local IPv4 is private'
assert_call_count 'curl ' 0 'local IPv6 success skips external probes'

reset_mocks
set_ip_mock 4 route \
  '1.1.1.1 via 192.168.1.1 dev eth0 src 192.168.1.20 uid 0'
set_ip_mock 4 addr \
  '2: eth0    inet 192.168.1.20/24 brd 192.168.1.255 scope global eth0\       valid_lft forever preferred_lft forever'
set_ip_mock 6 route \
  '2606:4700:4700::1111 from :: via fe80::1 dev eth0 src fe80::20 metric 1024 pref medium'
set_ip_mock 6 addr \
  '2: eth0    inet6 fe80::20/64 scope link\       valid_lft forever preferred_lft forever'
set_curl_mock 4 1 7 'temporary failure'
set_curl_mock 4 2 0 '1.1.1.1'
run_detect
assert_detect_ok '1.1.1.1' \
  'NAT private local IPv4 falls back to the second external IPv4 service'
assert_call_count 'curl ' 2 'IPv4 fallback tried services in order'
assert_call_count 'curl -4 ' 2 'IPv4 fallback used only the IPv4 endpoint family'

reset_mocks
set_curl_mock 4 1 18 '8.8.8.8'
set_curl_mock 4 2 0 $' \t1.1.1.1\r\n'
run_detect
assert_detect_ok '1.1.1.1' \
  'failed transfers are discarded even if they emitted a valid-looking partial IP'
assert_call_count 'curl ' 2 'partial failed response does not stop fallback'

reset_mocks
set_ip_mock 4 addr \
  $'2: eth0 inet 8.8.8.8/24 scope global deprecated eth0\n3: eth1 inet 1.1.1.1/24 scope global eth1'
run_detect
assert_detect_ok '1.1.1.1' 'deprecated interface addresses are skipped'
assert_call_count 'curl ' 0 'usable interface address does not require external probing'

reset_mocks
set_ip_mock 4 route \
  '1.1.1.1 via 192.168.1.1 dev eth0 src 192.168.1.20 uid 0'
set_ip_mock 4 addr \
  '2: eth0    inet 192.168.1.20/24 brd 192.168.1.255 scope global eth0\       valid_lft forever preferred_lft forever'
set_ip_mock 6 route \
  '2606:4700:4700::1111 from :: via fe80::1 dev eth0 src fe80::20 metric 1024 pref medium'
set_ip_mock 6 addr \
  '2: eth0    inet6 fe80::20/64 scope link\       valid_lft forever preferred_lft forever'
set_curl_mock 4 1 7 'IPv4 unavailable'
set_curl_mock 6 1 7 'IPv6 service unavailable'
set_curl_mock 6 2 0 '2606:4700:4700::1111'
run_detect
assert_detect_ok '2606:4700:4700::1111' \
  'external IPv6 falls back to the second HTTPS service'
assert_call_count 'curl -6 ' 2 'IPv6 fallback tried HTTPS services in order'

reset_mocks
set_ip_mock 4 route '' 1
set_ip_mock 4 addr '' 1
set_ip_mock 6 route '' 1
set_ip_mock 6 addr '' 1
set_curl_mock 4 1 7 'IPv4 unavailable'
set_curl_mock 6 1 7 'IPv6 unavailable'
run_detect
assert_detect_failure 1 'all local and external probes failing returns status 1'
assert_call_at_least 'curl -4 ' 1 'all-failure case attempted the IPv4 endpoint family'
assert_call_at_least 'curl -6 ' 1 'all-failure case attempted the IPv6 endpoint family'

reset_mocks
set_ip_mock 4 route \
  '1.1.1.1 via 192.168.1.1 dev eth0 src 192.168.1.20 uid 0'
set_ip_mock 4 addr \
  '2: eth0    inet 192.168.1.20/24 brd 192.168.1.255 scope global eth0\       valid_lft forever preferred_lft forever'
set_ip_mock 6 route \
  '2606:4700:4700::1111 from :: via fe80::1 dev eth0 src fe80::20 metric 1024 pref medium'
set_ip_mock 6 addr \
  '2: eth0    inet6 fe80::20/64 scope link\       valid_lft forever preferred_lft forever'
set_curl_mock 4 1 0 '192.0.2.1'
set_curl_mock 4 2 0 '203.0.113.9'
set_curl_mock 6 1 0 '2001:db8::1'
set_curl_mock 6 2 0 '3fff::1'
run_detect
assert_detect_failure 1 'reserved external addresses are never adopted'

reset_mocks
set_ip_mock 4 route \
  '1.1.1.1 via 192.168.1.1 dev eth0 src 192.168.1.20 uid 0'
set_ip_mock 4 addr \
  '2: eth0    inet 192.168.1.20/24 brd 192.168.1.255 scope global eth0\       valid_lft forever preferred_lft forever'
set_ip_mock 6 route \
  '2606:4700:4700::1111 from :: via fe80::1 dev eth0 src fe80::20 metric 1024 pref medium'
set_ip_mock 6 addr \
  '2: eth0    inet6 fe80::20/64 scope link\       valid_lft forever preferred_lft forever'
set_curl_mock 4 1 0 '<!doctype html><title>error</title>'
set_curl_mock 4 2 0 $'8.8.8.8\n1.1.1.1'
set_curl_mock 6 1 0 'example.com'
set_curl_mock 6 2 0 '2001:db8:::1'
run_detect
assert_detect_failure 1 'HTML, multiline, hostname, and malformed IP responses are skipped'

for value in '8.8.4.4' '2606:4700:4700::1111'; do
  reset_mocks
  run_detect "$value"
  assert_detect_ok "$value" "valid explicit override is returned unchanged"
  assert_call_count 'ip ' 0 'explicit override skips local address discovery'
  assert_call_count 'curl ' 0 'explicit override skips external probing'
done

for value in '10.10.10.10' '127.0.0.1' 'fd00::1' '::1'; do
  reset_mocks
  run_detect "$value"
  assert_detect_ok "$value" 'valid private or loopback explicit override is accepted'
  assert_call_count 'ip ' 0 'private explicit override skips local address discovery'
  assert_call_count 'curl ' 0 'private explicit override skips external probing'
done

for value in \
  '999.1.1.1' '01.2.3.4' '2001:db8:::1' '<html>8.8.8.8</html>' \
  $'8.8.8.8\n1.1.1.1' 'example.com'; do
  reset_mocks
  run_detect "$value"
  assert_detect_failure 2 'invalid explicit override returns status 2'
  assert_call_count 'ip ' 0 'invalid explicit override skips local address discovery'
  assert_call_count 'curl ' 0 'invalid explicit override skips external probing'
done

printf 'All public IP detection tests passed (%d checks); no network commands were run.\n' \
  "$checks"
