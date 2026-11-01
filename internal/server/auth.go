package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

const passwordIterations = 210000

type Credentials struct {
	Username   string `json:"username"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
}

func InitializeAuth(dir, username, password string, force bool) error {
	path := filepath.Join(dir, "auth.json")
	if _, err := os.Stat(path); err == nil && !force {
		return errors.New("管理员已初始化；如需重置请使用 --force")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(username) < 1 || len(username) > 64 {
		return errors.New("用户名长度应为 1–64")
	}
	if len(password) < 12 || len(password) > 256 {
		return errors.New("密码长度应为 12–256 字节")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return err
	}
	c := Credentials{username, base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key), passwordIterations}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return config.WritePrivate(path, append(b, '\n'))
}

func LoadAuth(dir string) (Credentials, error) {
	var c Credentials
	b, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return c, fmt.Errorf("读取管理员凭据失败，请先执行 relaydeck init: %w", err)
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, errors.New("管理员凭据格式错误")
	}
	salt, e1 := base64.StdEncoding.DecodeString(c.Salt)
	hash, e2 := base64.StdEncoding.DecodeString(c.Hash)
	if e1 != nil || e2 != nil || len(salt) != 32 || len(hash) != 32 || c.Iterations < 100000 || c.Iterations > 2000000 || c.Username == "" {
		return c, errors.New("管理员凭据无效")
	}
	return c, nil
}

func (c Credentials) Verify(username, password string) bool {
	if len(password) > 256 || len(username) > 64 {
		return false
	}
	salt, _ := base64.StdEncoding.DecodeString(c.Salt)
	expected, _ := base64.StdEncoding.DecodeString(c.Hash)
	actual, err := pbkdf2.Key(sha256.New, password, salt, c.Iterations, 32)
	return err == nil && subtle.ConstantTimeCompare(expected, actual) == 1 &&
		subtle.ConstantTimeCompare([]byte(c.Username), []byte(username)) == 1
}

type loginAttempt struct {
	failures int
	reset    time.Time
}

type sessions struct {
	mu       sync.Mutex
	tokens   map[[32]byte]time.Time
	attempts map[string]loginAttempt
}

func newSessions() *sessions {
	return &sessions{tokens: make(map[[32]byte]time.Time), attempts: make(map[string]loginAttempt)}
}

func (s *sessions) allow(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.attempts {
		if now.After(v.reset) {
			delete(s.attempts, k)
		}
	}
	a, ok := s.attempts[ip]
	return (!ok && len(s.attempts) < 4096) || (ok && a.failures < 5)
}

func (s *sessions) failed(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[ip]
	if time.Now().After(a.reset) {
		a = loginAttempt{reset: time.Now().Add(5 * time.Minute)}
	}
	a.failures++
	s.attempts[ip] = a
}

func (s *sessions) create(ip string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.tokens {
		if now.After(v) {
			delete(s.tokens, k)
		}
	}
	if len(s.tokens) >= 256 {
		return "", errors.New("活跃会话过多")
	}
	s.tokens[key] = now.Add(12 * time.Hour)
	delete(s.attempts, ip)
	return token, nil
}

func (s *sessions) valid(token string) bool {
	if len(token) != 64 {
		return false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.tokens[key]
	if ok && time.Now().After(expires) {
		delete(s.tokens, key)
		return false
	}
	return ok
}

func (s *sessions) remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, sha256.Sum256([]byte(token)))
}
