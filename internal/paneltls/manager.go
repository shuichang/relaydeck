// Package paneltls manages the panel certificate independently of forwarding
// rules. Handshakes use an immutable certificate through an atomic pointer;
// parsing and disk writes only happen when an administrator updates it or a
// source file changes.
package paneltls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

const (
	maxCertificateBytes = 512 << 10
	maxPrivateKeyBytes  = 64 << 10
	maxSettingsBytes    = 1 << 20
	reloadInterval      = time.Minute
)

type Options struct {
	DataDir         string
	DefaultCertFile string
	DefaultKeyFile  string
}

// Update contains write-only private material. It is never used in responses or
// the general configuration export.
type Update struct {
	Mode           string   `json:"mode"`
	CertificatePEM string   `json:"certificate_pem,omitempty"`
	PrivateKeyPEM  string   `json:"private_key_pem,omitempty"`
	CertPath       string   `json:"cert_path,omitempty"`
	KeyPath        string   `json:"key_path,omitempty"`
	Hosts          []string `json:"hosts,omitempty"`
	Days           int      `json:"days,omitempty"`
}

type CertificateInfo struct {
	Subject           string    `json:"subject"`
	Issuer            string    `json:"issuer"`
	DNSNames          []string  `json:"dns_names"`
	IPAddresses       []string  `json:"ip_addresses"`
	NotBefore         time.Time `json:"not_before"`
	NotAfter          time.Time `json:"not_after"`
	FingerprintSHA256 string    `json:"fingerprint_sha256"`
	SelfSigned        bool      `json:"self_signed"`
	ValidNow          bool      `json:"valid_now"`
}

type State struct {
	Available         bool             `json:"available"`
	Mode              string           `json:"mode"`
	Certificate       *CertificateInfo `json:"certificate"`
	CertPath          string           `json:"cert_path"`
	KeyPath           string           `json:"key_path"`
	AutoReloadSeconds int              `json:"auto_reload_seconds"`
	ReloadError       string           `json:"reload_error"`
	UpdatedAt         time.Time        `json:"updated_at"`
	Message           string           `json:"message,omitempty"`
}

// A single owner-only file commits both PEM values and their source atomically.
// File-backed certificates also retain a last known good snapshot, so a broken
// renewal cannot lock an administrator out after a restart.
type savedSettings struct {
	Version        int       `json:"version"`
	Mode           string    `json:"mode"`
	CertPath       string    `json:"cert_path,omitempty"`
	KeyPath        string    `json:"key_path,omitempty"`
	CertificatePEM string    `json:"certificate_pem"`
	PrivateKeyPEM  string    `json:"private_key_pem"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Manager struct {
	mu        sync.Mutex
	opts      Options
	path      string
	saved     savedSettings
	reloadErr string
	current   atomic.Pointer[tls.Certificate]
}

func New(opts Options) (*Manager, error) {
	m := &Manager{opts: opts, path: filepath.Join(opts.DataDir, "tls-settings.json")}
	data, err := readRegular(m.path, maxSettingsBytes)
	initial := errors.Is(err, os.ErrNotExist)
	switch {
	case initial:
		certPEM, keyPEM, readErr := readPair(opts.DefaultCertFile, opts.DefaultKeyFile)
		if readErr != nil {
			return nil, fmt.Errorf("读取默认面板证书失败: %w", readErr)
		}
		m.saved = savedSettings{Version: 1, Mode: "default", CertificatePEM: certPEM, PrivateKeyPEM: keyPEM, UpdatedAt: time.Now().UTC()}
	case err != nil:
		return nil, fmt.Errorf("读取面板 TLS 设置失败: %w", err)
	default:
		if err = json.Unmarshal(data, &m.saved); err != nil || m.saved.Version != 1 {
			return nil, errors.New("面板 TLS 设置文件格式或版本无效")
		}
		if err = m.validateSource(m.saved); err != nil {
			return nil, err
		}
	}
	// An expired but previously working certificate still lets the administrator
	// reach the settings page and replace it. New updates must be valid now.
	pair, err := parsePair(m.saved.CertificatePEM, m.saved.PrivateKeyPEM, false)
	if err != nil {
		return nil, fmt.Errorf("已保存的面板证书不可用: %w", err)
	}
	m.current.Store(pair)
	if initial {
		if err = m.commitLocked(m.saved, pair); err != nil {
			return nil, err
		}
	}
	if m.fileBacked() {
		// Failed renewal/read attempts retain the saved certificate.
		_ = m.reloadLocked()
	}
	return m, nil
}

func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	pair := m.current.Load()
	if pair == nil {
		return nil, errors.New("面板证书尚未加载")
	}
	return pair, nil
}

func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked()
}

func (m *Manager) stateLocked() State {
	certPath, keyPath := m.sourcePaths()
	state := State{
		Available: true, Mode: m.saved.Mode, CertPath: certPath, KeyPath: keyPath,
		ReloadError: m.reloadErr, UpdatedAt: m.saved.UpdatedAt,
	}
	if m.fileBacked() {
		state.AutoReloadSeconds = int(reloadInterval / time.Second)
	}
	if pair := m.current.Load(); pair != nil {
		leaf := pair.Leaf
		sum := sha256.Sum256(leaf.Raw)
		info := &CertificateInfo{
			Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(),
			DNSNames: append([]string{}, leaf.DNSNames...), IPAddresses: []string{},
			NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
			FingerprintSHA256: hex.EncodeToString(sum[:]),
			SelfSigned: bytes.Equal(leaf.RawIssuer, leaf.RawSubject) &&
				leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil,
			ValidNow: !time.Now().Before(leaf.NotBefore) && time.Now().Before(leaf.NotAfter),
		}
		for _, ip := range leaf.IPAddresses {
			info.IPAddresses = append(info.IPAddresses, ip.String())
		}
		for _, der := range pair.Certificate[1:] {
			intermediate, _ := x509.ParseCertificate(der) // All blocks were checked before activation.
			info.ValidNow = info.ValidNow && !time.Now().Before(intermediate.NotBefore) && time.Now().Before(intermediate.NotAfter)
		}
		state.Certificate = info
	}
	return state
}

func (m *Manager) Apply(update Update) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := savedSettings{Version: 1, Mode: update.Mode}
	if err := validateUpdate(update); err != nil {
		return m.stateLocked(), err
	}
	var err error
	switch update.Mode {
	case "default":
		next.CertificatePEM, next.PrivateKeyPEM, err = readPair(m.opts.DefaultCertFile, m.opts.DefaultKeyFile)
	case "files":
		next.CertPath = filepath.Clean(update.CertPath)
		next.KeyPath = filepath.Clean(update.KeyPath)
		next.CertificatePEM, next.PrivateKeyPEM, err = readPair(next.CertPath, next.KeyPath)
	case "pem":
		next.CertificatePEM, next.PrivateKeyPEM = update.CertificatePEM, update.PrivateKeyPEM
	case "self_signed":
		cert, key, generateErr := GenerateSelfSigned(update.Hosts, update.Days)
		err = generateErr
		next.CertificatePEM, next.PrivateKeyPEM = string(cert), string(key)
	}
	if err != nil {
		return m.stateLocked(), err
	}
	pair, err := parsePair(next.CertificatePEM, next.PrivateKeyPEM, true)
	if err != nil {
		return m.stateLocked(), err
	}
	if err = m.commitLocked(next, pair); err != nil {
		return m.stateLocked(), err
	}
	return m.stateLocked(), nil
}

func validateUpdate(u Update) error {
	hasPEM := u.CertificatePEM != "" || u.PrivateKeyPEM != ""
	hasPaths := u.CertPath != "" || u.KeyPath != ""
	hasGeneration := len(u.Hosts) != 0 || u.Days != 0
	switch u.Mode {
	case "default":
		if hasPEM || hasPaths || hasGeneration {
			return errors.New("恢复默认证书时不能提交其他证书参数")
		}
	case "pem":
		if hasPaths || hasGeneration || u.CertificatePEM == "" || u.PrivateKeyPEM == "" {
			return errors.New("请同时提供 PEM 证书链和未加密的 PEM 私钥")
		}
	case "files":
		if hasPEM || hasGeneration || !filepath.IsAbs(u.CertPath) || !filepath.IsAbs(u.KeyPath) ||
			filepath.Clean(u.CertPath) == filepath.Clean(u.KeyPath) ||
			strings.ContainsAny(u.CertPath+u.KeyPath, "\x00\r\n") {
			return errors.New("请提供两个不同的证书/私钥绝对路径；文件必须可由 RelayDeck 服务读取")
		}
	case "self_signed":
		if hasPEM || hasPaths {
			return errors.New("生成自签证书时不能提交 PEM 或文件路径")
		}
	default:
		return errors.New("证书模式必须为 default、pem、files 或 self_signed")
	}
	return nil
}

func (m *Manager) validateSource(saved savedSettings) error {
	switch saved.Mode {
	case "default", "pem", "self_signed":
		if saved.CertPath != "" || saved.KeyPath != "" {
			return errors.New("已保存的面板证书来源无效")
		}
	case "files":
		return validateUpdate(Update{Mode: "files", CertPath: saved.CertPath, KeyPath: saved.KeyPath})
	default:
		return errors.New("已保存的面板证书模式无效")
	}
	return nil
}

func (m *Manager) fileBacked() bool { return m.saved.Mode == "files" || m.saved.Mode == "default" }

func (m *Manager) sourcePaths() (string, string) {
	switch m.saved.Mode {
	case "default":
		return m.opts.DefaultCertFile, m.opts.DefaultKeyFile
	case "files":
		return m.saved.CertPath, m.saved.KeyPath
	default:
		return "", ""
	}
}

func (m *Manager) commitLocked(next savedSettings, pair *tls.Certificate) error {
	next.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return errors.New("编码面板 TLS 设置失败")
	}
	if len(data)+1 > maxSettingsBytes {
		return errors.New("面板 TLS 设置超过大小限制")
	}
	if err = config.WritePrivate(m.path, append(data, '\n')); err != nil {
		return fmt.Errorf("保存面板 TLS 设置失败，当前证书保持生效: %w", err)
	}
	m.saved = next
	m.reloadErr = ""
	m.current.Store(pair)
	return nil
}

func (m *Manager) Reload() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.reloadLocked()
	return m.stateLocked(), err
}

func (m *Manager) reloadLocked() error {
	if !m.fileBacked() {
		return errors.New("当前为面板托管证书，无需重新载入服务器文件")
	}
	certPath, keyPath := m.sourcePaths()
	certPEM, keyPEM, err := readPair(certPath, keyPath)
	var pair *tls.Certificate
	if err == nil {
		pair, err = parsePair(certPEM, keyPEM, true)
	}
	if err == nil && (certPEM != m.saved.CertificatePEM || keyPEM != m.saved.PrivateKeyPEM) {
		next := m.saved
		next.CertificatePEM, next.PrivateKeyPEM = certPEM, keyPEM
		err = m.commitLocked(next, pair)
	}
	if err != nil {
		m.reloadErr = fmt.Sprintf("证书重新载入失败，继续使用上一份证书: %v", err)
		return err
	}
	m.reloadErr = ""
	return nil
}

// Run checks renewed source files once per minute. It never interrupts active
// HTTP requests or forwarding connections.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(reloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			if m.fileBacked() {
				_ = m.reloadLocked()
			}
			m.mu.Unlock()
		}
	}
}

func readPair(certPath, keyPath string) (string, string, error) {
	cert, err := readRegular(certPath, maxCertificateBytes)
	if err != nil {
		return "", "", fmt.Errorf("读取证书文件失败: %w", err)
	}
	key, err := readRegular(keyPath, maxPrivateKeyBytes)
	if err != nil {
		return "", "", fmt.Errorf("读取私钥文件失败: %w", err)
	}
	return string(cert), string(key), nil
}

func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("必须使用普通文件")
	}
	if info.Size() > limit {
		return nil, errors.New("文件超过大小限制")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("文件超过大小限制")
	}
	return data, err
}

func parsePair(certPEM, keyPEM string, requireValid bool) (*tls.Certificate, error) {
	if len(certPEM) > maxCertificateBytes || len(keyPEM) > maxPrivateKeyBytes {
		return nil, errors.New("证书链应小于 512 KiB，私钥应小于 64 KiB")
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("证书与私钥无效或不匹配: %w", err)
	}
	if len(pair.Certificate) > 16 {
		return nil, errors.New("证书链不能超过 16 项")
	}
	chain := make([]*x509.Certificate, len(pair.Certificate))
	now := time.Now()
	for index, der := range pair.Certificate {
		chain[index], err = x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("证书链第 %d 项内容无效", index+1)
		}
		if requireValid && (now.Before(chain[index].NotBefore) || !now.Before(chain[index].NotAfter)) {
			return nil, fmt.Errorf("证书链第 %d 项已过期或尚未生效，请更换有效证书", index+1)
		}
		if index > 0 && (!bytes.Equal(chain[index-1].RawIssuer, chain[index].RawSubject) ||
			chain[index-1].CheckSignatureFrom(chain[index]) != nil) {
			return nil, fmt.Errorf("证书链第 %d 项不是上一项的有效签发者，请按叶证书、中间证书顺序提供完整链", index+1)
		}
	}
	leaf := chain[0]
	pair.Leaf = leaf
	if requireValid {
		if len(leaf.ExtKeyUsage) > 0 {
			serverAuth := false
			for _, usage := range leaf.ExtKeyUsage {
				serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny
			}
			if !serverAuth {
				return nil, errors.New("证书不允许用于 TLS 服务器认证")
			}
		}
		if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
			return nil, errors.New("证书必须包含域名或 IP 的 SAN")
		}
	}
	return &pair, nil
}
