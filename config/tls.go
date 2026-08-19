package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// TLSConfig configures TLS for an outbound infrastructure client.
type TLSConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	CAFile     string `mapstructure:"cafile"`
	CertFile   string `mapstructure:"certfile"`
	KeyFile    string `mapstructure:"keyfile"`
	ServerName string `mapstructure:"servername"`
}

// NatsAuthConfig configures one NATS authentication mechanism.
type NatsAuthConfig struct {
	Username        string `mapstructure:"username"`
	Password        string `mapstructure:"password"`
	Token           string `mapstructure:"token"`
	CredentialsFile string `mapstructure:"credentialsfile"`
}

// Validate checks the structural TLS contract without reading certificate
// files. path is the logical configuration path used in diagnostics.
func (c TLSConfig) Validate(path string) error {
	path = configValidationPath(path, "tls")
	fields := []struct {
		name  string
		value string
	}{
		{name: "cafile", value: c.CAFile},
		{name: "certfile", value: c.CertFile},
		{name: "keyfile", value: c.KeyFile},
		{name: "servername", value: c.ServerName},
	}

	var errs []error
	for _, field := range fields {
		if field.value != "" && strings.TrimSpace(field.value) == "" {
			errs = append(errs, fmt.Errorf("%s.%s must not be blank", path, field.name))
		}
		if !c.Enabled && field.value != "" {
			errs = append(errs, fmt.Errorf(
				"%s.enabled must be true when %s.%s is configured",
				path,
				path,
				field.name,
			))
		}
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		errs = append(errs, fmt.Errorf(
			"%s.certfile and %s.keyfile must be configured together",
			path,
			path,
		))
	}
	return errors.Join(errs...)
}

// Validate checks that NATS credentials select at most one complete
// authentication mechanism. Empty credentials retain anonymous access.
func (c NatsAuthConfig) Validate(path string) error {
	path = configValidationPath(path, "nats.auth")
	fields := []struct {
		name  string
		value string
	}{
		{name: "username", value: c.Username},
		{name: "password", value: c.Password},
		{name: "token", value: c.Token},
		{name: "credentialsfile", value: c.CredentialsFile},
	}

	var errs []error
	for _, field := range fields {
		if field.value != "" && strings.TrimSpace(field.value) == "" {
			errs = append(errs, fmt.Errorf("%s.%s must not be blank", path, field.name))
		}
	}

	usernamePassword := c.Username != "" || c.Password != ""
	if (c.Username == "") != (c.Password == "") {
		errs = append(errs, fmt.Errorf(
			"%s.username and %s.password must be configured together",
			path,
			path,
		))
	}
	modes := 0
	for _, configured := range []bool{
		usernamePassword,
		c.Token != "",
		c.CredentialsFile != "",
	} {
		if configured {
			modes++
		}
	}
	if modes > 1 {
		errs = append(errs, fmt.Errorf(
			"%s must configure at most one authentication mode: username/password, token, or credentialsfile",
			path,
		))
	}
	return errors.Join(errs...)
}

// LoadClientTLS builds a client TLS configuration. A nil configuration means
// TLS is disabled. File and parser failures intentionally report only the
// logical configuration field so credentials and local paths do not leak.
func LoadClientTLS(path string, cfg TLSConfig) (*tls.Config, error) {
	path = configValidationPath(path, "tls")
	if err := cfg.Validate(path); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.ServerName,
	}
	if cfg.CAFile != "" {
		caPEM, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("%s.cafile could not be read", path)
		}
		roots, ok := strictCertificatePool(caPEM)
		if !ok {
			return nil, fmt.Errorf("%s.cafile does not contain valid certificate-only PEM", path)
		}
		tlsConfig.RootCAs = roots
	}
	if cfg.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("%s.certfile and %s.keyfile must contain a valid client certificate and key", path, path)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil
}

func strictCertificatePool(data []byte) (*x509.CertPool, bool) {
	rest := bytes.TrimSpace(data)
	if len(rest) == 0 {
		return nil, false
	}

	pool := x509.NewCertPool()
	certificates := 0
	for len(rest) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, false
		}
		parsed, err := x509.ParseCertificates(block.Bytes)
		if err != nil || len(parsed) == 0 {
			return nil, false
		}
		for _, certificate := range parsed {
			pool.AddCert(certificate)
			certificates++
		}
		rest = bytes.TrimSpace(remaining)
	}
	return pool, certificates > 0
}

func configValidationPath(path, fallback string) string {
	if path = strings.TrimSpace(path); path == "" {
		return fallback
	}
	return path
}
