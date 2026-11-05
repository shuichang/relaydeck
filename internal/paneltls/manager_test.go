package paneltls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) (Options, string, string) {
	t.Helper()
	dir := t.TempDir()
	cert, key, err := GenerateSelfSigned([]string{"localhost", "127.0.0.1", "::1"}, 365)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{DataDir: dir, DefaultCertFile: filepath.Join(dir, "default.crt"), DefaultKeyFile: filepath.Join(dir, "default.key")}
	if err = os.WriteFile(opts.DefaultCertFile, cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(opts.DefaultKeyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	return opts, string(cert), string(key)
}

func freshPair(t *testing.T) (string, string) {
	t.Helper()
	cert, key, err := GenerateSelfSigned([]string{"localhost", "127.0.0.1", "::1"}, 365)
	if err != nil {
		t.Fatal(err)
	}
	return string(cert), string(key)
}

func testListener(t *testing.T, manager *Manager) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: manager.GetCertificate})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return listener.Addr().String()
}

func handshake(address string, roots *x509.CertPool) ([]byte, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{
		RootCAs: roots, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Raw, nil
}

func TestLiveRotationValidatesAndPersists(t *testing.T) {
	opts, cert1, _ := fixture(t)
	manager, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cert2, key2 := freshPair(t)
	_, wrongKey := freshPair(t)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(cert1))
	roots.AppendCertsFromPEM([]byte(cert2))
	address := testListener(t, manager)
	before, err := handshake(address, roots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Apply(Update{Mode: "pem", CertificatePEM: cert2, PrivateKeyPEM: wrongKey}); err == nil {
		t.Fatal("mismatched private key was accepted")
	}
	stillBefore, err := handshake(address, roots)
	if err != nil || !bytes.Equal(before, stillBefore) {
		t.Fatal("invalid update replaced the live certificate", err)
	}
	state, err := manager.Apply(Update{Mode: "pem", CertificatePEM: cert2, PrivateKeyPEM: key2})
	if err != nil {
		t.Fatal(err)
	}
	after, err := handshake(address, roots)
	if err != nil || bytes.Equal(before, after) {
		t.Fatal("new TLS connections did not receive the new certificate", err)
	}
	public, _ := json.Marshal(state)
	if bytes.Contains(public, []byte("PRIVATE KEY")) || bytes.Contains(public, []byte("private_key_pem")) ||
		bytes.Contains(public, []byte(key2)) || !state.Certificate.SelfSigned {
		t.Fatal("private material leaked into certificate metadata")
	}
	info, err := os.Stat(filepath.Join(opts.DataDir, "tls-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("stored private key must be owner-only")
	}
	_ = os.Remove(opts.DefaultCertFile)
	_ = os.Remove(opts.DefaultKeyFile)
	restarted, err := New(opts)
	if err != nil {
		t.Fatal("custom certificates must survive restart without the old CLI files", err)
	}
	loaded, _ := restarted.GetCertificate(nil)
	if !bytes.Equal(loaded.Leaf.Raw, after) {
		t.Fatal("restart lost the selected certificate")
	}
	if _, err = restarted.Apply(Update{Mode: "default"}); err == nil {
		t.Fatal("missing defaults must not replace the active certificate")
	}
}

func TestFileRenewalRetainsLastGoodPair(t *testing.T) {
	opts, _, _ := fixture(t)
	manager, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cert2, key2 := freshPair(t)
	certFile, keyFile := filepath.Join(opts.DataDir, "renewed.crt"), filepath.Join(opts.DataDir, "renewed.key")
	writeSource := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeSource(certFile, cert2)
	writeSource(keyFile, key2)
	if _, err = manager.Apply(Update{Mode: "files", CertPath: certFile, KeyPath: keyFile}); err != nil {
		t.Fatal(err)
	}
	old, _ := manager.GetCertificate(nil)
	cert3, key3 := freshPair(t)
	writeSource(certFile, cert3)
	if _, err = manager.Reload(); err == nil {
		t.Fatal("partially renewed key pair was accepted")
	}
	current, _ := manager.GetCertificate(nil)
	if current != old {
		t.Fatal("failed renewal changed the active certificate")
	}
	restarted, err := New(opts)
	if err != nil {
		t.Fatal("a partial renewal locked the panel out after restart", err)
	}
	if restarted.State().ReloadError == "" {
		t.Fatal("failed renewal was not reported")
	}
	current, _ = restarted.GetCertificate(nil)
	if !bytes.Equal(current.Leaf.Raw, old.Leaf.Raw) {
		t.Fatal("restart did not retain the saved known-good certificate")
	}
	writeSource(keyFile, key3)
	state, err := restarted.Reload()
	if err != nil || state.ReloadError != "" {
		t.Fatal("completed renewal could not be loaded", err)
	}
	current, _ = restarted.GetCertificate(nil)
	if bytes.Equal(current.Leaf.Raw, old.Leaf.Raw) {
		t.Fatal("completed renewal did not take effect")
	}
	_ = os.Remove(certFile)
	_ = os.Remove(keyFile)
	fallback, err := New(opts)
	if err != nil || fallback.State().ReloadError == "" {
		t.Fatal("deleted external files must preserve the managed snapshot", err)
	}
}

func TestDiskFailureDoesNotActivateUncommittedCertificate(t *testing.T) {
	opts, _, _ := fixture(t)
	manager, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := manager.GetCertificate(nil)
	if err = os.Remove(filepath.Join(opts.DataDir, "tls-settings.json")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(opts.DataDir, "tls-settings.json"), 0700); err != nil {
		t.Fatal(err)
	}
	cert, key := freshPair(t)
	if _, err = manager.Apply(Update{Mode: "pem", CertificatePEM: cert, PrivateKeyPEM: key}); err == nil {
		t.Fatal("persistence failure was hidden")
	}
	after, _ := manager.GetCertificate(nil)
	if before != after {
		t.Fatal("a certificate which cannot survive restart was activated")
	}
}

func TestFirstDefaultSnapshotSurvivesPartialRenewal(t *testing.T) {
	opts, _, _ := fixture(t)
	manager, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := manager.GetCertificate(nil)
	newCert, _ := freshPair(t)
	if err = os.WriteFile(opts.DefaultCertFile, []byte(newCert), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(opts)
	if err != nil {
		t.Fatal("partial renewal of startup defaults prevented restart", err)
	}
	recovered, _ := restarted.GetCertificate(nil)
	if !bytes.Equal(original.Leaf.Raw, recovered.Leaf.Raw) || restarted.State().ReloadError == "" {
		t.Fatal("initial default certificate was not retained as a fallback")
	}
}

func TestInvalidCertificateChainCannotBeSaved(t *testing.T) {
	opts, cert, key := fixture(t)
	manager, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := manager.GetCertificate(nil)
	storedBefore, err := os.ReadFile(manager.path)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedCert, _ := freshPair(t)
	for _, chain := range []string{
		cert + "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
		cert + unrelatedCert,
	} {
		if _, err = manager.Apply(Update{Mode: "pem", CertificatePEM: chain, PrivateKeyPEM: key}); err == nil {
			t.Fatal("malformed or unrelated intermediate certificate was accepted")
		}
		current, _ := manager.GetCertificate(nil)
		storedAfter, _ := os.ReadFile(manager.path)
		if current != original || !bytes.Equal(storedBefore, storedAfter) {
			t.Fatal("rejected certificate chain changed the live or persisted certificate")
		}
	}
}

func TestConcurrentHandshakesDuringRotation(t *testing.T) {
	opts, cert1, key1 := fixture(t)
	manager, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cert2, key2 := freshPair(t)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(cert1))
	roots.AppendCertsFromPEM([]byte(cert2))
	address := testListener(t, manager)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 24; i++ {
			if _, handshakeErr := handshake(address, roots); handshakeErr != nil {
				t.Error("rotation interrupted a TLS handshake", handshakeErr)
				return
			}
		}
	}()
	for i := 0; i < 6; i++ {
		cert, key := cert1, key1
		if i%2 == 0 {
			cert, key = cert2, key2
		}
		if _, err = manager.Apply(Update{Mode: "pem", CertificatePEM: cert, PrivateKeyPEM: key}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestExpiredCertificateCannotReplaceWorkingCertificate(t *testing.T) {
	opts, _, _ := fixture(t)
	manager, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := manager.GetCertificate(nil)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "expired"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	if _, err = manager.Apply(Update{Mode: "pem", CertificatePEM: certPEM, PrivateKeyPEM: keyPEM}); err == nil {
		t.Fatal("expired certificate was accepted")
	}
	current, _ := manager.GetCertificate(nil)
	if old != current {
		t.Fatal("expired certificate replaced the current one")
	}
	// A previously selected certificate expiring while the machine is offline
	// should still allow the administrator to access the repair UI.
	saved := savedSettings{Version: 1, Mode: "pem", CertificatePEM: certPEM, PrivateKeyPEM: keyPEM}
	data, _ := json.Marshal(saved)
	if err = os.WriteFile(filepath.Join(opts.DataDir, "tls-settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(opts)
	if err != nil || restarted.State().Certificate.ValidNow {
		t.Fatal("expired saved certificate should remain available for repair", err)
	}
}

func TestSelfSignedNamesAndInvalidSettings(t *testing.T) {
	cert, key, err := GenerateSelfSigned([]string{"localhost", "127.0.0.1", "::1", "*.example.com", "LOCALHOST"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := parsePair(string(cert), string(key), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "panel.example.com"} {
		if err = pair.Leaf.VerifyHostname(host); err != nil {
			t.Fatal(host, err)
		}
	}
	for _, host := range []string{"", "https://panel.example.com", "foo:443", "-bad.example", "foo..example", "../private", "::1%eth0"} {
		if _, _, err = GenerateSelfSigned([]string{host}, 365); err == nil {
			t.Errorf("invalid certificate host accepted: %q", host)
		}
	}
	opts, _, _ := fixture(t)
	manager, _ := New(opts)
	for _, update := range []Update{
		{Mode: "files", CertPath: "relative.crt", KeyPath: "relative.key"},
		{Mode: "pem", CertificatePEM: string(cert)},
		{Mode: "default", PrivateKeyPEM: string(key)},
		{Mode: "self_signed", Hosts: []string{"localhost"}, Days: 3651},
		{Mode: "unknown"},
	} {
		if _, err = manager.Apply(update); err == nil {
			t.Errorf("invalid TLS update accepted: mode=%s", update.Mode)
		}
	}
}
