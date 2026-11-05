package paneltls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

func GenerateSelfSigned(hosts []string, days int) ([]byte, []byte, error) {
	if days == 0 {
		days = 365
	}
	if days < 1 || days > 3650 {
		return nil, nil, errors.New("自签证书有效天数应为 1–3650")
	}
	if len(hosts) == 0 || len(hosts) > 64 {
		return nil, nil, errors.New("请填写 1–64 个证书域名或 IP")
	}
	template := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "RelayDeck"},
		NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().Add(time.Duration(days) * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	seen := make(map[string]bool)
	for _, input := range hosts {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input), "."))
		if seen[host] {
			continue
		}
		seen[host] = true
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if validDNSName(host) {
			template.DNSNames = append(template.DNSNames, host)
		} else {
			return nil, nil, fmt.Errorf("无效的证书域名或 IP: %q", input)
		}
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template.SerialNumber, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		return nil, nil, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), nil
}

func validDNSName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for index, label := range labels {
		if index == 0 && label == "*" && len(labels) > 2 {
			continue
		}
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
