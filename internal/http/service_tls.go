package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/open-rails/openrails/config"
)

// NewServiceTLSConfig builds the mTLS configuration for the standalone
// service-to-service listener.
func NewServiceTLSConfig(cfg config.ServiceMTLSConfig) (*tls.Config, error) {
	certFile := strings.TrimSpace(cfg.CertFile)
	keyFile := strings.TrimSpace(cfg.KeyFile)
	clientCAFile := strings.TrimSpace(cfg.ClientCAFile)

	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("service mTLS cert_file and key_file are required")
	}
	if clientCAFile == "" {
		return nil, fmt.Errorf("service mTLS client_ca_file is required")
	}

	cert, err := loadServiceCertificate(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load service mTLS certificate: %w", err)
	}

	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read service mTLS client CA file: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse service mTLS client CA file: no certificates found")
	}

	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := loadServiceCertificate(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, nil
}

func loadServiceCertificate(certFile, keyFile string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(certFile, keyFile)
}
