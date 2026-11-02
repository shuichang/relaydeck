package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestEngineInstallationRequiresSessionOriginCSRFAndEmptyObject(t *testing.T) {
	_, h, client, _ := testServer(t)
	endpoint := h.URL + "/api/engine/install"
	resp, _ := request(t, client, http.MethodPost, endpoint, `{}`, h.URL, true)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("installation allowed without authentication")
	}
	login(t, h, client)
	for _, tc := range []struct {
		body, origin string
		csrf         bool
		status       int
	}{
		{`{}`, h.URL, false, 403},
		{`{}`, "https://attacker.invalid", true, 403},
		{`{"url":"http://127.0.0.1"}`, h.URL, true, 400},
		{`{"force":true}`, h.URL, true, 400},
		{`null`, h.URL, true, 400},
		{`[]`, h.URL, true, 400},
		{`{} {}`, h.URL, true, 400},
		{strings.Repeat(" ", 1025) + `{}`, h.URL, true, 400},
		{`{}`, h.URL, true, 409}, // Test server has an explicit custom path.
	} {
		resp, raw := request(t, client, http.MethodPost, endpoint, tc.body, tc.origin, tc.csrf)
		if resp.StatusCode != tc.status {
			t.Errorf("install body=%q origin=%q: HTTP %d %s, expected %d", tc.body, tc.origin, resp.StatusCode, raw, tc.status)
		}
	}
}
