package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
	"github.com/shuichang/relaydeck/internal/engine"
	"github.com/shuichang/relaydeck/internal/forward"
	"github.com/shuichang/relaydeck/web"
)

type Options struct {
	DataDir      string
	Version      string
	EngineBinary string
}

type Server struct {
	mu           sync.Mutex
	opts         Options
	cfg          config.Config
	store        *config.Store
	auth         Credentials
	sessions     *sessions
	forward      *forward.Manager
	forwardError string
	engine       *engine.Manager
	logins       chan struct{}
	started      time.Time
	handler      http.Handler
}

func New(opts Options) (*Server, error) {
	auth, err := LoadAuth(opts.DataDir)
	if err != nil {
		return nil, err
	}
	store := config.NewStore(opts.DataDir)
	c, err := store.Load()
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts: opts, cfg: c, store: store, auth: auth, sessions: newSessions(),
		forward: forward.NewManager(), engine: engine.New(opts.DataDir, opts.EngineBinary), started: time.Now(),
		logins: make(chan struct{}, 2),
	}
	if err = s.forward.Apply(c.Rules); err != nil {
		s.forwardError = fmt.Sprintf("恢复转发规则失败: %v", err)
		log.Print(s.forwardError)
	}
	// A missing optional engine must not take the management UI or native forwarding down.
	if err = s.engine.Apply(c.Engine); err != nil {
		log.Printf("optional protocol engine unavailable: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.Handle("/api/", s.protect(http.HandlerFunc(s.api)))
	mux.Handle("/", http.FileServer(http.FS(web.FS())))
	s.handler = s.security(mux)
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Close() {
	s.forward.Close()
	s.engine.Close()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if media := strings.Split(r.Header.Get("Content-Type"), ";")[0]; media != "application/json" {
		fail(w, 415, "请使用 application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxFileSize)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "JSON 格式或字段无效")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fail(w, 400, "请求中只允许一个 JSON 对象")
		return false
	}
	return true
}

func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=86400")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if r.Header.Get("X-RelayDeck-CSRF") != "1" {
				fail(w, 403, "缺少 CSRF 请求头")
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "拒绝跨站请求")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) || u.User != nil {
					fail(w, 403, "请求来源与面板不一致")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("relaydeck_session")
		if err != nil || !s.sessions.valid(c.Value) {
			fail(w, 401, "请先登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip // Never trust spoofable X-Forwarded-For.
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	select {
	case s.logins <- struct{}{}:
		defer func() { <-s.logins }()
	default:
		fail(w, 429, "登录服务繁忙，请稍后重试")
		return
	}
	ip := clientIP(r)
	if !s.sessions.allow(ip) {
		w.Header().Set("Retry-After", "300")
		fail(w, 429, "登录尝试过多，请稍后重试")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.mu.Lock()
	auth := s.auth
	s.mu.Unlock()
	if !auth.Verify(body.Username, body.Password) {
		s.sessions.failed(ip)
		fail(w, 401, "用户名或密码错误")
		return
	}
	s.mu.Lock()
	if auth.Hash != s.auth.Hash {
		s.mu.Unlock()
		fail(w, 401, "凭据已更新，请重新登录")
		return
	}
	token, err := s.sessions.create(ip)
	s.mu.Unlock()
	if err != nil {
		fail(w, 503, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "relaydeck_session", Value: token, Path: "/", MaxAge: 12 * 3600, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]string{"username": auth.Username})
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "POST /api/logout":
		if c, err := r.Cookie("relaydeck_session"); err == nil {
			s.sessions.remove(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "relaydeck_session", Value: "", Path: "/", MaxAge: -1, Secure: r.TLS != nil, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "GET /api/status":
		s.status(w)
	case "GET /api/rules":
		rows := s.ruleSnapshots()
		if rows == nil {
			rows = []forward.Snapshot{}
		}
		writeJSON(w, 200, rows)
	case "PUT /api/rules":
		s.putRules(w, r)
	case "GET /api/engine":
		writeJSON(w, 200, s.engineState())
	case "GET /api/engine/templates":
		writeJSON(w, 200, engine.Templates())
	case "PUT /api/engine":
		s.putEngine(w, r)
	case "GET /api/config/export":
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Disposition", `attachment; filename="relaydeck-config.json"`)
		writeJSON(w, 200, s.cfg)
	case "POST /api/password":
		s.changePassword(w, r)
	default:
		fail(w, 404, "接口不存在")
	}
}

func (s *Server) status(w http.ResponseWriter) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	hostname, _ := os.Hostname()
	rows := s.ruleSnapshots()
	var active, connections, udp int64
	var in, out uint64
	for _, row := range rows {
		if row.Running {
			active++
		}
		connections += row.ActiveConnections
		udp += row.UDPSessions
		in += row.BytesIn
		out += row.BytesOut
	}
	es := s.engine.Snapshot()
	writeJSON(w, 200, map[string]any{
		"version": s.opts.Version, "uptime": int64(time.Since(s.started).Seconds()),
		"memory_bytes": m.Alloc, "memory_sys_bytes": m.Sys, "goroutines": runtime.NumGoroutine(),
		"go_version": runtime.Version(), "hostname": hostname,
		"forward": map[string]any{"total_rules": len(rows), "active_rules": active, "active_connections": connections, "udp_sessions": udp, "bytes_in": in, "bytes_out": out},
		"engine":  map[string]any{"enabled": es.Enabled, "available": es.Available, "running": es.Running, "version": es.Version, "error": es.Error},
	})
}

func (s *Server) engineState() engine.Snapshot {
	es := s.engine.Snapshot()
	s.mu.Lock()
	// Keep the saved configuration editable when the optional binary is missing
	// or cannot start after a reboot.
	es.Enabled = s.cfg.Engine.Enabled
	es.Config = append(json.RawMessage(nil), s.cfg.Engine.Config...)
	s.mu.Unlock()
	return es
}

func (s *Server) ruleSnapshots() []forward.Snapshot {
	rows := s.forward.Snapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forwardError == "" {
		return rows
	}
	rows = make([]forward.Snapshot, 0, len(s.cfg.Rules))
	for _, rule := range s.cfg.Rules {
		rule.AllowedCIDRs = append([]string(nil), rule.AllowedCIDRs...)
		row := forward.Snapshot{Rule: rule}
		if rule.Enabled {
			row.Error = s.forwardError
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) putRules(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Rules []config.Rule `json:"rules"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Rules == nil {
		b.Rules = []config.Rule{}
	}
	for i := range b.Rules {
		if b.Rules[i].ID == "" {
			id := make([]byte, 12)
			if _, err := rand.Read(id); err != nil {
				fail(w, 500, "生成规则 ID 失败")
				return
			}
			b.Rules[i].ID = hex.EncodeToString(id)
		}
		if b.Rules[i].MaxConnections == 0 {
			b.Rules[i].MaxConnections = 256
		}
		if b.Rules[i].UDPTimeout == 0 {
			b.Rules[i].UDPTimeout = 60
		}
	}
	if err := forward.ValidateRules(b.Rules); err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.cfg
	if err := s.forward.Apply(b.Rules); err != nil {
		fail(w, 409, err.Error())
		return
	}
	next := old
	next.Rules = b.Rules
	if err := s.store.Save(next); err != nil {
		rollbackRules := old.Rules
		if s.forwardError != "" {
			rollbackRules = nil
		}
		if rollback := s.forward.Apply(rollbackRules); rollback != nil {
			log.Printf("CRITICAL: config rollback failed: %v", rollback)
		}
		fail(w, 500, "配置写入失败，已尝试恢复原规则")
		return
	}
	s.cfg = next
	s.forwardError = ""
	writeJSON(w, 200, s.forward.Snapshot())
}

func (s *Server) putEngine(w http.ResponseWriter, r *http.Request) {
	var b config.Engine
	if !decode(w, r, &b) {
		return
	}
	if len(b.Config) == 0 || !json.Valid(b.Config) || len(b.Config) > 2<<20 || strings.TrimSpace(string(b.Config))[0] != '{' {
		fail(w, 400, "内核配置必须是 JSON 对象，且不超过 2 MiB")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.cfg
	if err := s.engine.Apply(b); err != nil {
		fail(w, 400, err.Error())
		return
	}
	next := old
	next.Engine = b
	if err := s.store.Save(next); err != nil {
		if rollback := s.engine.Apply(old.Engine); rollback != nil {
			log.Printf("CRITICAL: engine rollback failed: %v", rollback)
		}
		fail(w, 500, "配置写入失败，已尝试恢复原内核")
		return
	}
	s.cfg = next
	writeJSON(w, 200, s.engine.Snapshot())
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var b struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decode(w, r, &b) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.auth.Verify(s.auth.Username, b.OldPassword) {
		fail(w, 403, "当前密码错误")
		return
	}
	if err := InitializeAuth(s.opts.DataDir, s.auth.Username, b.NewPassword, true); err != nil {
		fail(w, 400, err.Error())
		return
	}
	auth, err := LoadAuth(s.opts.DataDir)
	if err != nil {
		fail(w, 500, "重新加载管理员凭据失败")
		return
	}
	s.auth = auth
	// Existing clients must log in again after a password change.
	s.sessions.mu.Lock()
	clear(s.sessions.tokens)
	s.sessions.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func Check(dataDir string) error {
	if _, err := LoadAuth(dataDir); err != nil {
		return err
	}
	c, err := config.NewStore(dataDir).Load()
	if err != nil {
		return err
	}
	if err = forward.ValidateRules(c.Rules); err != nil {
		return err
	}
	if len(c.Engine.Config) == 0 || !json.Valid(c.Engine.Config) {
		return errors.New("内核 JSON 配置无效")
	}
	return nil
}
