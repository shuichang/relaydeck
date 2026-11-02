package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	installVersion = "1.14.4"
	maxArchiveSize = 64 << 20
	maxBinarySize  = 160 << 20
	installTimeout = 5 * time.Minute
)

// Installation describes the single bounded background job. It contains no
// release redirect URLs or server filesystem paths.
type Installation struct {
	Supported       bool   `json:"supported"`
	Version         string `json:"version"`
	Phase           string `json:"phase"`
	Running         bool   `json:"running"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	Error           string `json:"error"`
}

type releaseAsset struct {
	name   string
	digest string
}

func officialAsset(goos, arch string) (releaseAsset, bool) {
	if goos != "linux" {
		return releaseAsset{}, false
	}
	digests := map[string]string{
		"amd64": "699a1b510aa4396b2176d32d1181920399ebf1e267835da6f3b598d0f7c24968",
		"arm64": "436441f76ee2f7fe21b5b2d426d3cb261a480455a704865789ad533e4273ec26",
		"arm":   "fd164ed45ebc784f657a3e1d0e0ad0f68afdcff864ee57122c2cfed3ddb80a00",
	}
	digest, ok := digests[arch]
	if arch == "arm" {
		arch = "armv7"
	}
	return releaseAsset{name: "sing-box-" + installVersion + "-linux-" + arch, digest: digest}, ok
}

type installer struct {
	mu      sync.Mutex
	manager *Manager
	state   Installation
	asset   releaseAsset
	client  *http.Client
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closed  bool
}

func newInstaller(manager *Manager) *installer {
	asset, supported := officialAsset(runtime.GOOS, runtime.GOARCH)
	return &installer{
		manager: manager,
		asset:   asset,
		state: Installation{
			Supported: supported && manager.binarySpec == "sing-box",
			Version:   installVersion, Phase: "idle",
		},
		client: &http.Client{
			Timeout: installTimeout,
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       30 * time.Second,
				MaxIdleConns:          2,
				MaxConnsPerHost:       2,
			},
			CheckRedirect: officialRedirect,
		},
	}
}

func officialRedirect(req *http.Request, via []*http.Request) error {
	host := req.URL.Hostname()
	if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil ||
		(req.URL.Port() != "" && req.URL.Port() != "443") ||
		(host != "github.com" && host != "release-assets.githubusercontent.com" && host != "objects.githubusercontent.com") {
		return errors.New("拒绝非官方 HTTPS 下载重定向")
	}
	return nil
}

func (i *installer) snapshot() Installation {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.state
}

// StartInstall starts at most one job. It neither changes nor starts an engine
// configuration. The server owns this job, rather than the HTTP request.
func (m *Manager) StartInstall() (Installation, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	i := m.installer
	i.mu.Lock()
	defer i.mu.Unlock()
	select {
	case <-m.closeCh:
		return i.state, errors.New("内核管理器已关闭")
	default:
	}
	if i.closed {
		return i.state, errors.New("内核管理器已关闭")
	}
	if !i.state.Supported {
		return i.state, errors.New("一键安装仅支持 Linux amd64、arm64、armv7 的默认内核路径；自定义内核路径请手动安装")
	}
	if i.state.Running {
		return i.state, nil
	}
	if _, err := m.ensureBinary(true); err == nil {
		return i.state, errors.New("内核已经安装，无需重复安装")
	}
	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	i.cancel = cancel
	i.state = Installation{Supported: true, Version: installVersion, Phase: "downloading", Running: true}
	i.wg.Add(1)
	go i.run(ctx, cancel)
	return i.state, nil
}

func (i *installer) close() {
	i.mu.Lock()
	i.closed = true
	if i.cancel != nil {
		i.cancel()
	}
	i.mu.Unlock()
	i.wg.Wait()
	i.client.CloseIdleConnections()
}

func (i *installer) phase(phase string) {
	i.mu.Lock()
	i.state.Phase = phase
	i.mu.Unlock()
}

func (i *installer) run(ctx context.Context, cancel context.CancelFunc) {
	defer i.wg.Done()
	defer cancel()
	err := i.install(ctx)
	i.mu.Lock()
	i.state.Running = false
	i.cancel = nil
	if err != nil {
		i.state.Phase = "failed"
		i.state.Error = safeInstallError(err)
	} else {
		i.state.Phase = "complete"
		i.state.Error = ""
	}
	message := i.state.Error
	i.mu.Unlock()
	if err != nil {
		i.manager.logs.append(-1, "内核安装失败: "+message)
	} else {
		i.manager.logs.append(-1, "sing-box "+installVersion+" 安装完成；填写配置后可保存并启用")
	}
}

func safeInstallError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "安装已取消"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "下载或安装超时，请检查服务器到 GitHub 的连接后重试"
	}
	// url.Error includes signed CDN query parameters. Never return it verbatim.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "下载官方内核失败，请检查服务器 DNS、HTTPS 和 GitHub 连接后重试"
	}
	// Filesystem errors may expose a private path.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "写入内核目录失败，请检查磁盘空间和面板服务权限"
	}
	return err.Error()
}

func (i *installer) install(ctx context.Context) error {
	dir := filepath.Join(i.manager.dataDir, "engines")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(dir, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	archivePath := filepath.Join(stage, "release.tar.gz")
	if err := i.download(ctx, archivePath); err != nil {
		return err
	}
	i.phase("verifying")
	if err := verifyArchive(archivePath, i.asset.digest); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	i.phase("extracting")
	binaryPath := filepath.Join(stage, "sing-box")
	if err := extractBinary(archivePath, i.asset.name+"/sing-box", binaryPath); err != nil {
		return err
	}
	i.phase("checking")
	version, err := binaryVersion(binaryPath)
	if err != nil || version != "sing-box version "+installVersion {
		return errors.New("下载的内核无法执行或版本不匹配")
	}
	m := i.manager
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-m.closeCh:
		return context.Canceled
	default:
	}
	// A manual install could have appeared during the download. Keep it.
	if _, err := m.ensureBinary(true); err == nil {
		return nil
	}
	target := filepath.Join(dir, "sing-box")
	if _, err := os.Lstat(target); err == nil {
		return errors.New("私有内核路径已存在但无法执行，请先检查该文件")
	} else if !os.IsNotExist(err) {
		return err
	}
	// Hard-link promotion is atomic, stays on this filesystem, and refuses to
	// overwrite any file/symlink installed concurrently by another process.
	if err := os.Link(binaryPath, target); err != nil {
		return err
	}
	if _, err := m.ensureBinary(true); err != nil {
		return errors.New("内核已写入但探测失败，请检查运行权限")
	}
	return nil
}

func (i *installer) download(ctx context.Context, path string) error {
	url := "https://github.com/SagerNet/sing-box/releases/download/v" + installVersion + "/" + i.asset.name + ".tar.gz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "RelayDeck-core-installer")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("官方内核下载返回 HTTP %d，请稍后重试", resp.StatusCode)
	}
	if resp.ContentLength > maxArchiveSize {
		return errors.New("官方内核归档超过下载大小限制")
	}
	i.mu.Lock()
	if resp.ContentLength > 0 {
		i.state.TotalBytes = resp.ContentLength
	}
	i.mu.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := io.LimitReader(resp.Body, maxArchiveSize+1)
	buf := make([]byte, 32<<10)
	var downloaded int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := reader.Read(buf)
		if n > 0 {
			downloaded += int64(n)
			if downloaded > maxArchiveSize {
				return errors.New("官方内核归档超过下载大小限制")
			}
			if _, err := file.Write(buf[:n]); err != nil {
				return err
			}
			i.mu.Lock()
			i.state.DownloadedBytes = downloaded
			i.mu.Unlock()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("下载连接中断，请稍后重试")
		}
	}
	if downloaded == 0 || (resp.ContentLength >= 0 && downloaded != resp.ContentLength) {
		return errors.New("官方内核下载不完整，请重试")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func verifyArchive(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxArchiveSize+1))
	if err != nil {
		return err
	}
	if size > maxArchiveSize || hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("官方内核 SHA-256 校验失败，已拒绝安装，请重试")
	}
	return nil
}

func extractBinary(archivePath, member, target string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return errors.New("官方内核归档格式无效")
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, maxBinarySize+(8<<20)))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return errors.New("官方归档缺少预期的 sing-box 文件")
		}
		if err != nil {
			return errors.New("读取官方内核归档失败")
		}
		if header.Name != member {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > maxBinarySize || strings.Contains(member, "..") {
			return errors.New("官方内核归档成员类型或大小无效")
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			return err
		}
		defer out.Close()
		n, err := io.CopyN(out, reader, header.Size)
		if err != nil || n != header.Size {
			return errors.New("官方内核二进制解压不完整")
		}
		if err := out.Sync(); err != nil {
			return err
		}
		return out.Close()
	}
}
