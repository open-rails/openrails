package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestNewServiceTLSConfigMutualTLSHandshake(t *testing.T) {
	now := time.Now().UTC()
	caCert, caKey, caPEM := testCA(t, "openrails-test-ca", now.Add(-time.Hour), now.Add(time.Hour))
	serverCertPEM, serverKeyPEM := testLeaf(t, caCert, caKey, "openrails", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, nil, now.Add(-time.Hour), now.Add(time.Hour))
	clientCertPEM, clientKeyPEM := testLeaf(t, caCert, caKey, "doujins.internal", []string{"doujins.internal"}, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Hour), now.Add(time.Hour))
	expiredClientCertPEM, expiredClientKeyPEM := testLeaf(t, caCert, caKey, "expired.internal", []string{"expired.internal"}, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-2*time.Hour), now.Add(-time.Hour))
	otherCACert, otherCAKey, _ := testCA(t, "other-ca", now.Add(-time.Hour), now.Add(time.Hour))
	untrustedClientCertPEM, untrustedClientKeyPEM := testLeaf(t, otherCACert, otherCAKey, "other.internal", []string{"other.internal"}, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Hour), now.Add(time.Hour))

	dir := t.TempDir()
	serverCertFile := writeTestFile(t, dir, "server.crt", serverCertPEM)
	serverKeyFile := writeTestFile(t, dir, "server.key", serverKeyPEM)
	caFile := writeTestFile(t, dir, "ca.crt", caPEM)

	tlsConfig, err := NewServiceTLSConfig(config.ServiceMTLSConfig{
		CertFile:     serverCertFile,
		KeyFile:      serverKeyFile,
		ClientCAFile: caFile,
	})
	require.NoError(t, err)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.TLS.PeerCertificates)
		w.WriteHeader(http.StatusNoContent)
	}))
	ts.TLS = tlsConfig
	ts.StartTLS()
	t.Cleanup(ts.Close)

	validClientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	require.NoError(t, err)
	expiredClientCert, err := tls.X509KeyPair(expiredClientCertPEM, expiredClientKeyPEM)
	require.NoError(t, err)
	untrustedClientCert, err := tls.X509KeyPair(untrustedClientCertPEM, untrustedClientKeyPEM)
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, getWithClientCert(t, ts.URL, caPEM, validClientCert))
	requireRequestFails(t, ts.URL, caPEM)
	requireRequestFails(t, ts.URL, caPEM, expiredClientCert)
	requireRequestFails(t, ts.URL, caPEM, untrustedClientCert)
}

func TestNewServiceTLSConfigReloadsServerCertificate(t *testing.T) {
	now := time.Now().UTC()
	caCert, caKey, caPEM := testCA(t, "openrails-test-ca", now.Add(-time.Hour), now.Add(time.Hour))
	firstCertPEM, firstKeyPEM := testLeaf(t, caCert, caKey, "openrails-one", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, nil, now.Add(-time.Hour), now.Add(time.Hour))
	secondCertPEM, secondKeyPEM := testLeaf(t, caCert, caKey, "openrails-two", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, nil, now.Add(-time.Hour), now.Add(time.Hour))

	dir := t.TempDir()
	serverCertFile := writeTestFile(t, dir, "server.crt", firstCertPEM)
	serverKeyFile := writeTestFile(t, dir, "server.key", firstKeyPEM)
	caFile := writeTestFile(t, dir, "ca.crt", caPEM)

	tlsConfig, err := NewServiceTLSConfig(config.ServiceMTLSConfig{
		CertFile:     serverCertFile,
		KeyFile:      serverKeyFile,
		ClientCAFile: caFile,
	})
	require.NoError(t, err)

	firstLoaded, err := tlsConfig.GetCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, firstCertPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: firstLoaded.Certificate[0]}))

	require.NoError(t, os.WriteFile(serverCertFile, secondCertPEM, 0600))
	require.NoError(t, os.WriteFile(serverKeyFile, secondKeyPEM, 0600))

	secondLoaded, err := tlsConfig.GetCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, secondCertPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: secondLoaded.Certificate[0]}))
}

func getWithClientCert(t *testing.T, url string, caPEM []byte, certs ...tls.Certificate) int {
	t.Helper()
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(caPEM))
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      roots,
				ServerName:   "localhost",
				Certificates: certs,
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func requireRequestFails(t *testing.T, url string, caPEM []byte, certs ...tls.Certificate) {
	t.Helper()
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(caPEM))
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      roots,
				ServerName:   "localhost",
				Certificates: certs,
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(url)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
}

func testCA(t *testing.T, cn string, notBefore, notAfter time.Time) (*x509.Certificate, *rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          testSerial(t),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testLeaf(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, cn string, dnsNames []string, ips []net.IP, usages []x509.ExtKeyUsage, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: testSerial(t),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usages,
	}
	if len(tmpl.ExtKeyUsage) == 0 {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func testSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	return serial
}

func writeTestFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0600))
	return path
}
