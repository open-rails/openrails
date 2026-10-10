package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testCAPEM is a throwaway self-signed CA certificate.
func testCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestRedisOptions(t *testing.T) {
	ca := testCAPEM(t)

	opts, err := RedisOptions(&RedisConfig{URL: "rediss://cache.example:6380/2", Username: "openrails", Password: "s3cret", CACert: ca})
	require.NoError(t, err)
	require.Equal(t, []any{"cache.example:6380", 2, "openrails", "s3cret"}, []any{opts.Addr, opts.DB, opts.Username, opts.Password})
	require.NotNil(t, opts.TLSConfig)
	require.Equal(t, "cache.example", opts.TLSConfig.ServerName)
	require.NotNil(t, opts.TLSConfig.RootCAs)

	opts, err = RedisOptions(&RedisConfig{URL: "redis://user:pw@cache:6379/1"})
	require.NoError(t, err)
	require.Equal(t, []any{"cache:6379", 1, "user", "pw"}, []any{opts.Addr, opts.DB, opts.Username, opts.Password})
	require.Nil(t, opts.TLSConfig)

	opts, err = RedisOptions(&RedisConfig{Addr: "10.0.0.5:6380", Username: "openrails", CACert: ca})
	require.NoError(t, err)
	require.Equal(t, "10.0.0.5", opts.TLSConfig.ServerName, "a CA implies TLS")
	opts, err = RedisOptions(&RedisConfig{Addr: "cache.example:6380", TLS: true})
	require.NoError(t, err)
	require.Nil(t, opts.TLSConfig.RootCAs, "the system roots")

	for want, c := range map[string]RedisConfig{
		"set redis.addr":       {Password: "x"},
		"exclusive":            {Addr: "a:1", URL: "redis://a:1"},
		"drop redis.db":        {URL: "redis://a:1", DB: 2},
		"use a rediss:// URL":  {URL: "redis://a:1", CACert: ca},
		"no PEM certificate":   {Addr: "a:1", CACert: "not a certificate"},
		"missing port":         {Addr: "cache"},
		"invalid URL scheme":   {URL: "http://a:1"},
		"redis.url: parse \"r": {URL: "redis://user:hunter2@a:1/%zz"},
	} {
		_, err := RedisOptions(&c)
		require.ErrorContains(t, err, want)
		require.NotContains(t, err.Error(), "hunter2", "a password never reaches an error")
	}
}
