package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

const (
	maxEngineConfigBytes = 2 << 20
	checkTimeout         = 12 * time.Second
	versionTimeout       = 3 * time.Second
	startupGracePeriod   = 600 * time.Millisecond
	stopGracePeriod      = 3 * time.Second
	maxRestartAttempts   = 5
)

var defaultEngineConfig = json.RawMessage(`{}`)

// Snapshot is the engine state exposed by the HTTP API.
type Snapshot struct {
	Enabled   bool            `json:"enabled"`
	Config    json.RawMessage `json:"config"`
	Available bool            `json:"available"`
	Running   bool            `json:"running"`
	Version   string          `json:"version"`
	Error     string          `json:"error"`
	Logs      []string        `json:"logs"`
}

// Manager supervises one optional sing-box child process. It does not persist
// the relaydeck configuration.
type Manager struct {
	dataDir    string
	binarySpec string

	opMu sync.Mutex
	mu   sync.Mutex

	binaryPath  string
	available   bool
	version     string
	lastProbe   time.Time
	probeMu     sync.Mutex
	desired     config.Engine
	configBytes []byte
	configPath  string
	proc        *managedProcess
	revision    uint64
	errorText   string
	closed      bool
	closeOnce   sync.Once
	closeCh     chan struct{}
	logs        *logRing
}

type managedProcess struct {
	cmd      *exec.Cmd
	exited   chan struct{}
	exitErr  error
	stopping bool
}

// New creates an optional engine manager. An empty binary name resolves
// "sing-box" with LookPath. A missing binary leaves the engine unavailable
// without affecting native forwarding.
func New(dataDir, binary string) *Manager {
	if strings.TrimSpace(binary) == "" {
		binary = "sing-box"
	}
	if strings.TrimSpace(dataDir) == "" {
		dataDir = filepath.Join(os.TempDir(), "relaydeck-engine")
	}
	manager := &Manager{
		dataDir:    filepath.Clean(dataDir),
		binarySpec: binary,
		closeCh:    make(chan struct{}),
		logs:       newLogRing(defaultLogCapacity),
	}
	_, _ = manager.ensureBinary(true)
	return manager
}

// Apply validates a complete engine configuration and supervises it. A failed
// configuration check or process start leaves the previous process and config
// active whenever the previous process can be restored.
func (m *Manager) Apply(candidate config.Engine) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if m.isClosed() {
		return errors.New("sing-box 管理器已关闭")
	}

	canonical, err := normalizeConfig(candidate.Config)
	if err != nil {
		m.setError(err.Error())
		return err
	}

	m.mu.Lock()
	oldEngine := cloneEngine(m.desired)
	oldConfig := append([]byte(nil), m.configBytes...)
	oldProcess := m.proc
	m.mu.Unlock()

	sameConfig := bytes.Equal(canonical, oldConfig)
	skipCheck := !candidate.Enabled && (sameConfig || bytes.Equal(canonical, defaultEngineConfig))
	newSecrets := secretsFromConfig(canonical)
	oldSecrets := secretsFromConfig(oldConfig)
	m.logs.replaceSecrets(mergeSecrets(oldSecrets, newSecrets))

	if skipCheck {
		return m.applyWithoutStart(candidate, canonical, oldEngine, oldProcess)
	}

	binaryPath, err := m.ensureBinary(false)
	if err != nil {
		safeError := redactText(err.Error(), mergeSecrets(oldSecrets, newSecrets))
		m.setError(safeError)
		return errors.New(safeError)
	}

	checkPath, err := m.createPrivateConfig(canonical)
	if err != nil {
		safeError := redactText("创建 sing-box 临时配置失败: "+err.Error(), mergeSecrets(oldSecrets, newSecrets))
		m.setError(safeError)
		return errors.New(safeError)
	}
	removeCheckPath := true
	defer func() {
		if removeCheckPath {
			_ = os.Remove(checkPath)
		}
	}()

	if err = m.checkConfig(binaryPath, checkPath, mergeSecrets(oldSecrets, newSecrets)); err != nil {
		m.setError(err.Error())
		m.logs.append(-1, "配置校验失败: "+err.Error())
		return err
	}

	if !candidate.Enabled {
		if oldProcess != nil {
			if stopErr := m.stopProcess(oldProcess); stopErr != nil {
				safeError := redactText("停止 sing-box 失败: "+stopErr.Error(), mergeSecrets(oldSecrets, newSecrets))
				m.setError(safeError)
				return errors.New(safeError)
			}
		}
		return m.commitDisabled(candidate, canonical, checkPath, newSecrets)
	}

	if sameConfig && oldProcess != nil && processStillRunning(oldProcess) {
		m.mu.Lock()
		m.desired.Enabled = true
		m.errorText = ""
		m.mu.Unlock()
		m.logs.replaceSecrets(newSecrets)
		return nil
	}

	if err = m.ensurePrivateConfig(checkPath, canonical); err != nil {
		safeError := redactText("准备 sing-box 运行配置失败: "+err.Error(), mergeSecrets(oldSecrets, newSecrets))
		m.setError(safeError)
		return errors.New(safeError)
	}

	if oldProcess != nil {
		if stopErr := m.stopProcess(oldProcess); stopErr != nil {
			safeError := redactText("停止旧 sing-box 失败: "+stopErr.Error(), mergeSecrets(oldSecrets, newSecrets))
			m.setError(safeError)
			return errors.New(safeError)
		}
	}

	newProcess, startErr := m.startAndWait(binaryPath, checkPath, canonical, mergeSecrets(oldSecrets, newSecrets))
	if startErr == nil && !processStillRunning(newProcess) {
		startErr = errors.New(redactText("sing-box 启动后立即退出: "+processExitText(newProcess.exitErr), mergeSecrets(oldSecrets, newSecrets)))
	}
	if startErr == nil {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			_ = m.stopProcess(newProcess)
			return errors.New("sing-box 管理器已关闭")
		}
		m.proc = newProcess
		m.desired = cloneEngine(candidate)
		m.configBytes = append([]byte(nil), canonical...)
		oldPath := m.configPath
		m.configPath = checkPath
		m.revision++
		m.errorText = ""
		deleteOldPath := oldPath != "" && oldPath != checkPath
		m.mu.Unlock()

		removeCheckPath = false
		if deleteOldPath {
			_ = os.Remove(oldPath)
		}
		m.logs.replaceSecrets(newSecrets)
		m.logs.append(-1, "sing-box 已启动")
		go m.observeProcess(newProcess)
		return nil
	}

	safeStartError := redactText(startErr.Error(), mergeSecrets(oldSecrets, newSecrets))
	rollbackErr := m.restorePrevious(oldEngine, oldConfig, binaryPath, oldSecrets, newSecrets)
	if rollbackErr != nil {
		safeError := safeStartError + "；旧配置已恢复，但旧 sing-box 重启失败: " + rollbackErr.Error()
		m.setError(safeError)
		m.logs.append(-1, "应用失败且回滚异常: "+safeError)
		return errors.New(safeError)
	}
	m.setError(safeStartError)
	m.logs.append(-1, "应用失败，已回滚旧配置: "+safeStartError)
	return errors.New(safeStartError)
}

func (m *Manager) applyWithoutStart(candidate config.Engine, canonical []byte, oldEngine config.Engine, oldProcess *managedProcess) error {
	if oldProcess != nil {
		if err := m.stopProcess(oldProcess); err != nil {
			safeError := redactText("停止 sing-box 失败: "+err.Error(), secretsFromConfig(oldEngine.Config))
			m.setError(safeError)
			return errors.New(safeError)
		}
	}
	return m.commitDisabled(candidate, canonical, "", secretsFromConfig(canonical))
}

func (m *Manager) commitDisabled(candidate config.Engine, canonical []byte, removePath string, secrets []string) error {
	m.mu.Lock()
	oldPath := m.configPath
	m.desired = cloneEngine(candidate)
	m.configBytes = append([]byte(nil), canonical...)
	m.configPath = ""
	m.revision++
	m.errorText = ""
	m.mu.Unlock()

	if oldPath != "" {
		_ = os.Remove(oldPath)
	}
	if removePath != "" && removePath != oldPath {
		_ = os.Remove(removePath)
	}
	m.logs.replaceSecrets(secrets)
	m.logs.append(-1, "sing-box 已停用")
	return nil
}

func (m *Manager) restorePrevious(oldEngine config.Engine, oldConfig []byte, binaryPath string, oldSecrets, newSecrets []string) error {
	m.mu.Lock()
	oldPath := m.configPath
	m.desired = cloneEngine(oldEngine)
	m.configBytes = append([]byte(nil), oldConfig...)
	m.mu.Unlock()

	m.logs.replaceSecrets(mergeSecrets(oldSecrets, newSecrets))

	if !oldEngine.Enabled || len(strings.TrimSpace(string(oldConfig))) == 0 || bytes.Equal(oldConfig, defaultEngineConfig) {
		m.mu.Lock()
		m.proc = nil
		m.errorText = ""
		m.mu.Unlock()
		return nil
	}
	if oldPath == "" {
		var err error
		oldPath, err = m.createPrivateConfig(oldConfig)
		if err != nil {
			return err
		}
	}
	if err := m.ensurePrivateConfig(oldPath, oldConfig); err != nil {
		return err
	}
	previous, err := m.startAndWait(binaryPath, oldPath, oldConfig, mergeSecrets(oldSecrets, newSecrets))
	if err == nil && !processStillRunning(previous) {
		err = errors.New(redactText("旧 sing-box 配置启动后立即退出: "+processExitText(previous.exitErr), mergeSecrets(oldSecrets, newSecrets)))
	}
	if err != nil {
		m.mu.Lock()
		m.proc = nil
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.proc = previous
	m.configPath = oldPath
	m.mu.Unlock()
	m.logs.replaceSecrets(oldSecrets)
	m.logs.append(-1, "旧 sing-box 配置已恢复并重新启动")
	go m.observeProcess(previous)
	return nil
}

// Snapshot returns a detached copy suitable for concurrent API reads.
func (m *Manager) Snapshot() Snapshot {
	m.maybeProbeMissingBinary()

	m.mu.Lock()
	snapshot := Snapshot{
		Enabled:   m.desired.Enabled,
		Config:    cloneRawMessage(m.configBytes),
		Available: m.available,
		Running:   m.proc != nil,
		Version:   m.version,
		Error:     m.errorText,
	}
	m.mu.Unlock()
	if len(snapshot.Config) == 0 {
		snapshot.Config = cloneRawMessage(defaultEngineConfig)
	}
	snapshot.Logs = m.logs.snapshot()
	return snapshot
}

// Close stops only the child process owned by this manager and removes its
// current private runtime config.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.closeCh)
		m.opMu.Lock()
		defer m.opMu.Unlock()

		m.mu.Lock()
		m.closed = true
		m.desired.Enabled = false
		process := m.proc
		path := m.configPath
		m.configPath = ""
		m.mu.Unlock()

		if process != nil {
			if err := m.stopProcess(process); err != nil {
				m.logs.append(-1, "关闭 sing-box 失败: "+err.Error())
			}
		}
		m.mu.Lock()
		m.proc = nil
		m.revision++
		m.errorText = ""
		m.mu.Unlock()
		if path != "" {
			_ = os.Remove(path)
		}
	})
}

func (m *Manager) observeProcess(process *managedProcess) {
	select {
	case <-process.exited:
	case <-m.closeCh:
		return
	}
	m.logs.flushPartial()

	m.mu.Lock()
	if m.proc != process {
		m.mu.Unlock()
		return
	}
	m.proc = nil
	stopping := process.stopping
	enabled := m.desired.Enabled
	closed := m.closed
	revision := m.revision
	errorText := processExitText(process.exitErr)
	secrets := secretsFromConfig(m.configBytes)
	if !stopping && enabled && !closed {
		m.errorText = redactText("sing-box 意外退出: "+errorText+"，正在自动重启", secrets)
	}
	m.mu.Unlock()

	if stopping || !enabled || closed {
		return
	}
	m.logs.replaceSecrets(secrets)
	m.logs.append(-1, "sing-box 意外退出: "+errorText+"; 将按退避策略自动重启")
	go m.restartLoop(revision)
}

func (m *Manager) restartLoop(revision uint64) {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	var lastError string
	for attempt := 0; attempt < maxRestartAttempts; attempt++ {
		delay := delays[attempt]
		if !m.waitRetry(delay) {
			return
		}

		m.opMu.Lock()
		m.mu.Lock()
		if m.closed || !m.desired.Enabled || m.revision != revision || m.proc != nil {
			m.mu.Unlock()
			m.opMu.Unlock()
			return
		}
		configBytes := append([]byte(nil), m.configBytes...)
		configPath := m.configPath
		m.mu.Unlock()

		m.mu.Lock()
		if m.available {
			m.errorText = redactText(
				fmt.Sprintf("sing-box 意外退出，%s 后执行自动重启 (%d/%d)", delay, attempt+1, maxRestartAttempts),
				secretsFromConfig(configBytes),
			)
		}
		m.mu.Unlock()

		binaryPath, err := m.ensureBinary(false)
		if err == nil && configPath == "" {
			configPath, err = m.createPrivateConfig(configBytes)
		}
		if err == nil {
			err = m.ensurePrivateConfig(configPath, configBytes)
		}
		var process *managedProcess
		if err == nil {
			process, err = m.startAndWait(binaryPath, configPath, configBytes, secretsFromConfig(configBytes))
		}
		if err == nil && !processStillRunning(process) {
			err = errors.New(redactText("sing-box 自动重启后立即退出: "+processExitText(process.exitErr), secretsFromConfig(configBytes)))
		}
		if err != nil {
			lastError = redactText(err.Error(), secretsFromConfig(configBytes))
			m.setError(lastError)
			m.opMu.Unlock()
			continue
		}

		m.mu.Lock()
		if m.closed || !m.desired.Enabled || m.revision != revision {
			m.mu.Unlock()
			_ = m.stopProcess(process)
			m.opMu.Unlock()
			return
		}
		m.proc = process
		if m.configPath == "" {
			m.configPath = configPath
		}
		m.errorText = ""
		m.mu.Unlock()
		m.logs.append(-1, fmt.Sprintf("sing-box 自动重启成功 (%d/%d)", attempt+1, maxRestartAttempts))
		go m.observeProcess(process)
		m.opMu.Unlock()
		return
	}
	if lastError == "" {
		lastError = "进程未能保持运行"
	}
	m.setError("sing-box 连续自动重启 5 次失败: " + lastError)
	m.logs.append(-1, "sing-box 自动重启已停止: "+lastError)
}

func (m *Manager) waitRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-m.closeCh:
		return false
	}
}

func (m *Manager) startAndWait(binaryPath, configPath string, configBytes []byte, secrets []string) (*managedProcess, error) {
	process, err := m.startProcess(binaryPath, configPath)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(startupGracePeriod)
	defer timer.Stop()
	select {
	case <-process.exited:
		m.logs.flushPartial()
		return nil, errors.New(redactText("sing-box 启动后立即退出: "+processExitText(process.exitErr), secrets))
	case <-timer.C:
		return process, nil
	case <-m.closeCh:
		_ = m.stopProcess(process)
		return nil, errors.New("sing-box 启动期间管理器已关闭")
	}
}

func (m *Manager) startProcess(binaryPath, configPath string) (*managedProcess, error) {
	cmd := exec.Command(binaryPath, "run", "-c", configPath)
	cmd.Dir = m.dataDir
	cmd.Stdout = m.logs.stdoutWriter()
	cmd.Stderr = m.logs.stderrWriter()
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 sing-box 失败: %w", err)
	}

	process := &managedProcess{
		cmd:    cmd,
		exited: make(chan struct{}),
	}

	go func() {
		process.exitErr = cmd.Wait()
		m.logs.flushPartial()
		close(process.exited)
	}()
	return process, nil
}

func (m *Manager) stopProcess(process *managedProcess) error {
	if process == nil {
		return nil
	}
	m.mu.Lock()
	process.stopping = true
	m.mu.Unlock()
	select {
	case <-process.exited:
		return nil
	default:
	}

	if err := terminateProcess(process.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// Continue to the forced-kill path below.
	}
	timer := time.NewTimer(stopGracePeriod)
	defer timer.Stop()
	select {
	case <-process.exited:
		return nil
	case <-timer.C:
	}

	killErr := forceKillProcess(process.cmd.Process)
	select {
	case <-process.exited:
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("强制终止 sing-box: %w", killErr)
		}
		return nil
	case <-time.After(2 * time.Second):
		if killErr != nil {
			return fmt.Errorf("sing-box 未退出且强制终止失败: %w", killErr)
		}
		return errors.New("sing-box 在强制终止后仍未退出")
	}
}

func (m *Manager) checkConfig(binaryPath, configPath string, secrets []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	output := &cappedBuffer{limit: 32 << 10}
	cmd := exec.CommandContext(ctx, binaryPath, "check", "-c", configPath)
	cmd.Dir = m.dataDir
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if err == nil {
		if text := strings.TrimSpace(output.String()); text != "" {
			m.logs.append(-1, "sing-box check: "+text)
		}
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("sing-box check 超时（%s）", checkTimeout)
	}
	detail := strings.TrimSpace(output.String())
	if detail == "" {
		detail = err.Error()
	}
	return errors.New(redactText("sing-box check 验证失败: "+detail, secrets))
}

func (m *Manager) createPrivateConfig(content []byte) (string, error) {
	if err := os.MkdirAll(m.dataDir, 0700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(m.dataDir, ".sing-box-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return "", err
	}
	if _, err = file.Write(content); err != nil {
		return "", err
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func (m *Manager) ensurePrivateConfig(path string, content []byte) error {
	if path == "" {
		return errors.New("sing-box 配置临时路径为空")
	}
	matches := false
	if existing, err := os.ReadFile(path); err == nil {
		matches = bytes.Equal(existing, content)
	}
	if !matches {
		if err := os.WriteFile(path, content, 0600); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0600)
}

func (m *Manager) ensureBinary(force bool) (string, error) {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()

	m.mu.Lock()
	if !force && m.available && m.binaryPath != "" {
		path := m.binaryPath
		m.mu.Unlock()
		return path, nil
	}
	m.mu.Unlock()

	path, version, err := m.probeBinary()
	m.mu.Lock()
	m.lastProbe = time.Now()
	if err != nil {
		m.available = false
		m.binaryPath = ""
		m.version = ""
		m.mu.Unlock()
		return "", err
	}
	m.available = true
	m.binaryPath = path
	m.version = version
	m.mu.Unlock()
	return path, nil
}

func (m *Manager) probeBinary() (string, string, error) {
	path, err := exec.LookPath(m.binarySpec)
	if err != nil {
		return "", "", fmt.Errorf("未找到 sing-box 可执行文件 %q", m.binarySpec)
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()

	output := &cappedBuffer{limit: 8 << 10}
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", "", fmt.Errorf("读取 sing-box 版本超时（%s）", versionTimeout)
		}
		detail := strings.TrimSpace(output.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", "", fmt.Errorf("无法运行 sing-box: %s", redactText(detail, nil))
	}
	version := strings.TrimSpace(output.String())
	if line, _, found := strings.Cut(version, "\n"); found {
		version = strings.TrimSpace(line)
	}
	if version == "" {
		version = "sing-box（版本输出为空）"
	}
	return path, version, nil
}

func (m *Manager) maybeProbeMissingBinary() {
	m.mu.Lock()
	shouldProbe := !m.available && !m.closed && time.Since(m.lastProbe) >= 5*time.Second
	m.mu.Unlock()
	if shouldProbe {
		_, _ = m.ensureBinary(true)
	}
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *Manager) setError(text string) {
	m.mu.Lock()
	secrets := secretsFromConfig(m.configBytes)
	m.errorText = redactText(text, secrets)
	m.mu.Unlock()
}

func normalizeConfig(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return append([]byte(nil), defaultEngineConfig...), nil
	}
	if len(raw) > maxEngineConfigBytes {
		return nil, fmt.Errorf("sing-box 配置超过 %d 字节", maxEngineConfigBytes)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, errors.New("sing-box 配置不是有效 JSON")
	}
	result := compact.Bytes()
	if len(result) == 0 || result[0] != '{' {
		return nil, errors.New("sing-box 配置必须是 JSON 对象")
	}
	return append([]byte(nil), result...), nil
}

func cloneEngine(engine config.Engine) config.Engine {
	return config.Engine{
		Enabled: engine.Enabled,
		Config:  cloneRawMessage(engine.Config),
	}
}

func cloneRawMessage(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func processStillRunning(process *managedProcess) bool {
	select {
	case <-process.exited:
		return false
	default:
		return true
	}
}

func processExitText(err error) string {
	if err == nil {
		return "进程已退出"
	}
	return err.Error()
}

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := b.buf.String()
	if b.truncated {
		result += "\n…[输出已截断]"
	}
	return result
}

var _ io.Writer = (*cappedBuffer)(nil)
