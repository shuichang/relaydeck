package engine

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	defaultLogCapacity = 200
	maxLogLineBytes    = 4096
)

type logRing struct {
	mu        sync.Mutex
	capacity  int
	lines     []string
	next      int
	full      bool
	pending   [2][]byte
	truncated [2]bool
	secrets   []string
}

func newLogRing(capacity int) *logRing {
	if capacity <= 0 {
		capacity = defaultLogCapacity
	}
	return &logRing{
		capacity: capacity,
		lines:    make([]string, capacity),
	}
}

func (r *logRing) stdoutWriter() *logStreamWriter {
	return &logStreamWriter{ring: r, stream: 0}
}

func (r *logRing) stderrWriter() *logStreamWriter {
	return &logStreamWriter{ring: r, stream: 1}
}

type logStreamWriter struct {
	ring   *logRing
	stream int
}

func (w *logStreamWriter) Write(p []byte) (int, error) {
	w.ring.write(w.stream, p)
	return len(p), nil
}

func (r *logRing) write(stream int, p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for len(p) > 0 {
		if index := bytes.IndexByte(p, '\n'); index >= 0 {
			r.appendPendingLocked(stream, p[:index])
			r.flushPendingLocked(stream)
			p = p[index+1:]
			continue
		}
		r.appendPendingLocked(stream, p)
		return
	}
}

func (r *logRing) appendPendingLocked(stream int, p []byte) {
	if len(p) == 0 {
		return
	}
	remaining := maxLogLineBytes - len(r.pending[stream])
	if remaining <= 0 {
		r.truncated[stream] = true
		return
	}
	if len(p) > remaining {
		r.pending[stream] = append(r.pending[stream], p[:remaining]...)
		r.truncated[stream] = true
		return
	}
	r.pending[stream] = append(r.pending[stream], p...)
}

func (r *logRing) flushPendingLocked(stream int) {
	line := strings.TrimSuffix(string(r.pending[stream]), "\r")
	if r.truncated[stream] {
		line += " …[已截断]"
	}
	r.pending[stream] = r.pending[stream][:0]
	r.truncated[stream] = false
	r.appendLocked(stream, line)
}

func (r *logRing) append(stream int, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendLocked(stream, line)
}

func (r *logRing) appendLocked(stream int, line string) {
	line = redactText(line, r.secrets)
	if len(line) > maxLogLineBytes+len(" …[已截断]") {
		line = line[:maxLogLineBytes] + " …[已截断]"
	}
	prefix := "[stdout] "
	if stream == 1 {
		prefix = "[stderr] "
	} else if stream < 0 {
		prefix = ""
	}
	r.lines[r.next] = prefix + line
	r.next = (r.next + 1) % r.capacity
	if r.next == 0 {
		r.full = true
	}
}

func (r *logRing) flushPartial() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for stream := range r.pending {
		if len(r.pending[stream]) > 0 || r.truncated[stream] {
			r.flushPendingLocked(stream)
		}
	}
}

func (r *logRing) replaceSecrets(secrets []string) {
	r.mu.Lock()
	r.secrets = append(r.secrets[:0], secrets...)
	r.mu.Unlock()
}

func (r *logRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := r.next
	if r.full {
		count = r.capacity
	}
	result := make([]string, 0, count)
	if !r.full {
		for i := 0; i < r.next; i++ {
			result = append(result, r.lines[i])
		}
		return result
	}
	for i := 0; i < r.capacity; i++ {
		result = append(result, r.lines[(r.next+i)%r.capacity])
	}
	return result
}

var (
	jsonSecretPattern = regexp.MustCompile(`(?i)("(?:password|passwd|passphrase|uuid|private_key|private_key_passphrase|pre_shared_key|psk|token|client_secret|secret)"\s*:\s*)"(?:\\.|[^"\\])*"`)
	textSecretPattern = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|uuid|private_key|private_key_passphrase|pre_shared_key|psk|token|client_secret|secret)\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)
	urlUserPattern    = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

var sensitiveKeys = map[string]struct{}{
	"password":               {},
	"passwd":                 {},
	"passphrase":             {},
	"private_key":            {},
	"private_key_passphrase": {},
	"pre_shared_key":         {},
	"psk":                    {},
	"token":                  {},
	"client_secret":          {},
	"secret":                 {},
	"uuid":                   {},
	"username":               {},
	"user":                   {},
}

func secretsFromConfig(config json.RawMessage) []string {
	var value any
	if json.Unmarshal(config, &value) != nil {
		return nil
	}
	set := make(map[string]struct{})
	collectSecrets(value, set)
	secrets := make([]string, 0, len(set))
	for secret := range set {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	return secrets
}

func collectSecrets(value any, set map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, sensitive := sensitiveKeys[strings.ToLower(key)]; sensitive {
				if text, ok := child.(string); ok && text != "" {
					set[text] = struct{}{}
				}
			}
			collectSecrets(child, set)
		}
	case []any:
		for _, child := range typed {
			collectSecrets(child, set)
		}
	}
}

func redactText(text string, secrets []string) string {
	text = jsonSecretPattern.ReplaceAllString(text, `$1"[REDACTED]"`)
	text = textSecretPattern.ReplaceAllString(text, `$1=[REDACTED]`)
	text = urlUserPattern.ReplaceAllString(text, `$1[REDACTED]@`)
	for _, secret := range secrets {
		if len(secret) < 3 || strings.Contains(secret, "[REDACTED]") {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}

func mergeSecrets(first, second []string) []string {
	set := make(map[string]struct{}, len(first)+len(second))
	for _, secret := range first {
		if secret != "" {
			set[secret] = struct{}{}
		}
	}
	for _, secret := range second {
		if secret != "" {
			set[secret] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for secret := range set {
		result = append(result, secret)
	}
	sort.Slice(result, func(i, j int) bool {
		return len(result[i]) > len(result[j])
	})
	return result
}
