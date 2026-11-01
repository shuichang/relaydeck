package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
	"github.com/shuichang/relaydeck/internal/server"
)

var version = "dev"
var commit = "unknown"

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println("RelayDeck — 轻量 TCP/UDP 转发面板")
		fmt.Println("用法: relaydeck <serve|init|certificate|check|version> [参数]")
		return nil
	}
	switch args[0] {
	case "version", "--version", "-version":
		fmt.Printf("relaydeck %s (%s)\n", version, commit)
		return nil
	case "init":
		f := flag.NewFlagSet("init", flag.ContinueOnError)
		dir := f.String("data", "/var/lib/relaydeck", "数据目录")
		username := f.String("username", "admin", "管理员用户名")
		stdin := f.Bool("password-stdin", false, "从标准输入读取密码")
		force := f.Bool("force", false, "重置已有管理员密码")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if !*stdin {
			return errors.New("请使用 --password-stdin 从标准输入安全地传入密码")
		}
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 259))
		if err != nil {
			return err
		}
		password := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if err = server.InitializeAuth(*dir, *username, password, *force); err != nil {
			return err
		}
		fmt.Println("管理员已初始化；密码仅保存为加盐哈希。")
		return nil
	case "check":
		f := flag.NewFlagSet("check", flag.ContinueOnError)
		dir := f.String("data", "/var/lib/relaydeck", "数据目录")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if err := server.Check(*dir); err != nil {
			return err
		}
		fmt.Println("配置格式有效。端口可用性及协议内核完整校验在启动或保存时执行。")
		return nil
	case "certificate":
		return certificate(args[1:])
	case "serve":
		return serve(args[1:])
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func serve(args []string) error {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := f.String("data", "/var/lib/relaydeck", "数据目录")
	listen := f.String("listen", ":9443", "管理面板监听地址")
	cert := f.String("tls-cert", "/etc/relaydeck/tls.crt", "TLS 证书文件")
	key := f.String("tls-key", "/etc/relaydeck/tls.key", "TLS 私钥文件")
	engine := f.String("engine-binary", "sing-box", "可选协议内核路径")
	localHTTP := f.Bool("insecure-local", false, "仅在回环地址允许 HTTP，用于本地开发")
	if err := f.Parse(args); err != nil {
		return err
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(96 << 20)
	}
	if *localHTTP {
		host, _, err := net.SplitHostPort(*listen)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return errors.New("--insecure-local 只允许 IP 回环监听地址")
		}
	}
	s, err := server.New(server.Options{DataDir: *dir, Version: version, EngineBinary: *engine})
	if err != nil {
		return err
	}
	defer s.Close()
	h := &http.Server{
		Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if !*localHTTP {
		if _, err = tls.LoadX509KeyPair(*cert, *key); err != nil {
			return fmt.Errorf("TLS 证书不可用（可使用 relaydeck certificate 创建）: %w", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() {
		if *localHTTP {
			done <- h.ListenAndServe()
		} else {
			done <- h.ListenAndServeTLS(*cert, *key)
		}
	}()
	scheme := "https"
	if *localHTTP {
		scheme = "http"
	}
	log.Printf("RelayDeck %s listening at %s://%s", version, scheme, *listen)
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err = h.Shutdown(shutdown); err != nil {
			_ = h.Close()
		}
	}
	return nil
}

func certificate(args []string) error {
	f := flag.NewFlagSet("certificate", flag.ContinueOnError)
	cert := f.String("cert", "/etc/relaydeck/tls.crt", "证书输出文件")
	key := f.String("key", "/etc/relaydeck/tls.key", "私钥输出文件")
	hosts := f.String("hosts", "localhost,127.0.0.1,::1", "证书 IP 和域名，以逗号分隔")
	days := f.Int("days", 365, "有效天数")
	force := f.Bool("force", false, "覆盖已有证书")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *days < 1 || *days > 3650 {
		return errors.New("有效天数应为 1–3650")
	}
	if *cert == *key {
		return errors.New("证书和私钥不能使用相同文件")
	}
	for _, p := range []string{*cert, *key} {
		if _, err := os.Stat(p); err == nil && !*force {
			return fmt.Errorf("%s 已存在，覆盖需要 --force", p)
		}
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "RelayDeck"},
		NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().Add(time.Duration(*days) * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range strings.Split(*hosts, ",") {
		host = strings.TrimSpace(host)
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if host != "" && len(host) <= 253 && !strings.ContainsAny(host, " /\\\n\r\t:") {
			template.DNSNames = append(template.DNSNames, host)
		} else {
			return fmt.Errorf("无效证书主机 %q", host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		return err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	if err = config.WritePrivate(*key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})); err != nil {
		return err
	}
	if err = config.WritePrivate(*cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return err
	}
	fmt.Println("已生成自签名证书；公网浏览器首次访问需要确认或导入信任。")
	return nil
}
