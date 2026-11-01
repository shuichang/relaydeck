#!/usr/bin/env bash
set -euo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)" || exit 1
installer="${script_dir}/install.sh"
tmpdir="$(mktemp -d)"
server_pid=""
other_pid=""

cleanup() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if [[ -n "$other_pid" ]]; then
    kill "$other_pid" 2>/dev/null || true
    wait "$other_pid" 2>/dev/null || true
  fi
  rm -rf -- "$tmpdir"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

pass() {
  printf 'PASS: %s\n' "$*"
}

for cmd in awk curl go openssl sed ss tar; do
  command -v "$cmd" >/dev/null 2>&1 || fail "required test command not found: $cmd"
done

sed -n '/^# BEGIN HEALTH CHECK HELPERS$/,/^# END HEALTH CHECK HELPERS$/p' \
  "$installer" > "${tmpdir}/helpers.sh"
[[ -s "${tmpdir}/helpers.sh" ]] || fail "health helper block was not found"
# shellcheck source=/dev/null
source "${tmpdir}/helpers.sh"

test_active=1
test_main_pid=""
systemctl() {
  case "${1:-}" in
    is-active)
      [[ "$test_active" -eq 1 ]]
      ;;
    show)
      [[ -n "$test_main_pid" ]] || return 1
      printf '%s\n' "$test_main_pid"
      ;;
    *)
      return 1
      ;;
  esac
}

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj '/CN=localhost' -addext 'subjectAltName=DNS:localhost' \
  -keyout "${tmpdir}/tls.key" -out "${tmpdir}/tls.crt" \
  >/dev/null 2>&1

cat > "${tmpdir}/test-helper.go" <<'GO'
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

const healthBody = `{"status":"ok","service":"relaydeck","version":"v0.1.1"}`

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: test-helper serve CERT KEY PORT_FILE | fixture ARCHIVE")
	}
	switch os.Args[1] {
	case "serve":
		if len(os.Args) != 5 {
			log.Fatal("serve requires CERT KEY PORT_FILE")
		}
		serve(os.Args[2], os.Args[3], os.Args[4])
	case "fixture":
		if len(os.Args) != 3 {
			log.Fatal("fixture requires ARCHIVE")
		}
		if err := makeFixture(os.Args[2]); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal("unknown mode")
	}
}

func serve(certPath, keyPath, portPath string) {
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		log.Fatal(err)
	}
	var listener net.Listener
	for attempt := 0; attempt < 5; attempt++ {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
		if listener.Addr().(*net.TCPAddr).Port != 9443 {
			break
		}
		_ = listener.Close()
		listener = nil
	}
	if listener == nil {
		log.Fatal("ephemeral port repeatedly selected forbidden port 9443")
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(portPath, []byte(port), 0600); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, healthBody)
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err := server.Serve(tlsListener); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func makeFixture(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	entries := []string{"sing-box-test/linux/sing-box"}
	for index := 0; index < 5000; index++ {
		entries = append(entries, fmt.Sprintf("filler/%05d", index))
	}
	for _, name := range entries {
		header := &tar.Header{
			Name: name,
			Mode: 0600,
			Size: 0,
		}
		if err = tarWriter.WriteHeader(header); err != nil {
			break
		}
	}
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gzipWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
GO

GO111MODULE=off go build -trimpath \
  -o "${tmpdir}/test-helper" "${tmpdir}/test-helper.go"

"${tmpdir}/test-helper" serve \
  "${tmpdir}/tls.crt" "${tmpdir}/tls.key" "${tmpdir}/port" &
server_pid=$!

for _ in {1..100}; do
  [[ -s "${tmpdir}/port" ]] && break
  kill -0 "$server_pid" 2>/dev/null || fail "isolated HTTPS test server exited"
  sleep 0.05
done
[[ -s "${tmpdir}/port" ]] || fail "isolated HTTPS test server did not become ready"
test_port="$(< "${tmpdir}/port")"
[[ "$test_port" =~ ^[0-9]+$ ]] || fail "isolated HTTPS test server returned an invalid port"
[[ "$test_port" != "9443" ]] || fail "isolated test must never use port 9443"

for _ in {1..100}; do
  if curl -kfsS --max-time 1 "https://localhost:${test_port}/healthz" \
      --output "${tmpdir}/initial-health.json" 2>/dev/null; then
    break
  fi
  sleep 0.05
done
[[ -s "${tmpdir}/initial-health.json" ]] || fail "isolated HTTPS test server did not answer"

relaydeck_port_owned_by_pid "$test_port" "$server_pid" \
  || fail "listen socket should belong to the isolated HTTPS server"
pass "listen socket ownership matches the intended PID"

sleep 30 &
other_pid=$!

test_main_pid="$other_pid"
# shellcheck disable=SC2310 # the probe returns explicitly and is expected to fail here
if relaydeck_health_probe relaydeck.service "$test_port" v0.1.1; then
  fail "a healthy HTTPS response from a different socket owner was accepted"
fi
pass "unrelated HTTPS service on the same port is rejected"

test_main_pid="$server_pid"
relaydeck_health_probe relaydeck.service "$test_port" v0.1.1 \
  || fail "matching unit PID, listener, and health identity should pass"
pass "MainPID, listener ownership, and service/version identity all match"

# shellcheck disable=SC2310 # the probe returns explicitly and is expected to fail here
if relaydeck_health_probe relaydeck.service "$test_port" v9.9.9; then
  fail "health response with the wrong version was accepted"
fi
pass "health version mismatch is rejected"

test_active=0
# shellcheck disable=SC2310 # the probe returns explicitly and is expected to fail here
if relaydeck_health_probe relaydeck.service "$test_port" v0.1.1; then
  fail "inactive unit was accepted"
fi
pass "inactive unit is rejected"

test_active=1
wait_for_relaydeck_health relaydeck.service "$test_port" v0.1.1 8 2 \
  || fail "consecutive stable health probes should pass"
pass "health verification requires consecutive stable samples"

test_main_pid="$other_pid"
# shellcheck disable=SC2310 # the sustained probe is expected to fail here
if wait_for_relaydeck_health relaydeck.service "$test_port" v0.1.1 8 2; then
  fail "changing/unowned MainPID was accepted"
fi
pass "unstable or foreign MainPID never passes the sustained check"

"${tmpdir}/test-helper" fixture "${tmpdir}/fixture.tar.gz"

member="$(
  tar -tzf "${tmpdir}/fixture.tar.gz" |
    awk -v name="sing-box-test/linux/sing-box" '
      $0 == name && !found { found = $0 }
      END { print found }
    '
)"
[[ "$member" == "sing-box-test/linux/sing-box" ]] \
  || fail "large archive listing did not find the expected member"
pass "archive member lookup consumes tar output without SIGPIPE"

printf 'All isolated installer tests passed; relaydeck port 9443 was not used.\n'
