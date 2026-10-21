package postgres

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vanclief/ez"
)

// writeTestCA writes a self-signed CA certificate as PEM and returns its path.
func writeTestCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "compose test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "ca.pem")
	err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	require.NoError(t, err)

	return path
}

func TestBuildDSNEscapesCredentials(t *testing.T) {
	cfg := &ConnectionConfig{
		Username: "app@user",
		Password: "p#a/s?s%w:o@rd",
		Host:     "db.example.com:5432",
		Database: "compose",
	}

	dsn := buildDSN(cfg, "require")

	parsed, err := url.Parse(dsn)
	require.NoError(t, err)

	password, ok := parsed.User.Password()
	assert.True(t, ok)
	assert.Equal(t, cfg.Password, password)
	assert.Equal(t, cfg.Username, parsed.User.Username())
	assert.Equal(t, cfg.Host, parsed.Host)
	assert.Equal(t, "/compose", parsed.Path)
	assert.Equal(t, "require", parsed.Query().Get("sslmode"))
	assert.Empty(t, parsed.Query().Get("sslrootcert"))

	// pgdriver panicked on such passwords before the DSN was escaped.
	var connector *pgdriver.Connector
	assert.NotPanics(t, func() {
		connector = pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	})
	assert.Equal(t, cfg.Password, connector.Config().Password)
	assert.Equal(t, cfg.Username, connector.Config().User)
}

func TestResolveSSLMode(t *testing.T) {
	caPath := writeTestCA(t)

	notPEM := filepath.Join(t.TempDir(), "not.pem")
	err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600)
	require.NoError(t, err)

	tests := []struct {
		name    string
		cfg     ConnectionConfig
		want    string
		wantErr bool
	}{
		{name: "SSL false", cfg: ConnectionConfig{}, want: "disable"},
		{name: "SSL true", cfg: ConnectionConfig{SSL: true}, want: "require"},
		{name: "SSLMode overrides SSL", cfg: ConnectionConfig{SSL: true, SSLMode: "disable"}, want: "disable"},
		{name: "require", cfg: ConnectionConfig{SSLMode: "require"}, want: "require"},
		{name: "verify-full with cert", cfg: ConnectionConfig{SSLMode: "verify-full", SSLRootCert: caPath}, want: "verify-full"},
		{name: "verify-ca with cert", cfg: ConnectionConfig{SSLMode: "verify-ca", SSLRootCert: caPath}, want: "verify-ca"},
		{name: "verify-full without cert uses system pool", cfg: ConnectionConfig{SSLMode: "verify-full"}, want: "verify-full"},
		{name: "verify-ca without cert uses system pool", cfg: ConnectionConfig{SSLMode: "verify-ca"}, want: "verify-ca"},
		{name: "verify-full with missing cert file", cfg: ConnectionConfig{SSLMode: "verify-full", SSLRootCert: filepath.Join(t.TempDir(), "missing.pem")}, wantErr: true},
		{name: "verify-full with non-PEM cert", cfg: ConnectionConfig{SSLMode: "verify-full", SSLRootCert: notPEM}, wantErr: true},
		{name: "bogus mode", cfg: ConnectionConfig{SSLMode: "bogus"}, wantErr: true},
		{name: "prefer is not allowed", cfg: ConnectionConfig{SSLMode: "prefer"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSSLMode(&tc.cfg)
			if tc.wantErr {
				require.Error(t, err)
				assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestConnectToDatabaseRejectsInvalidSSLMode(t *testing.T) {
	_, err := ConnectToDatabase(&ConnectionConfig{
		Username: "postgres",
		Host:     "localhost:5432",
		Database: "compose_test",
		SSLMode:  "bogus",
	})
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestBuildDSNVerifyFullWithoutCertUsesSystemPool(t *testing.T) {
	cfg := &ConnectionConfig{
		Username: "postgres",
		Password: "secret",
		Host:     "db.example.com:5432",
		Database: "compose",
		SSLMode:  "verify-full",
	}

	sslmode, err := resolveSSLMode(cfg)
	require.NoError(t, err)

	dsn := buildDSN(cfg, sslmode)

	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	assert.False(t, parsed.Query().Has("sslrootcert"))

	tlsConfig := pgdriver.NewConnector(pgdriver.WithDSN(dsn)).Config().TLSConfig
	require.NotNil(t, tlsConfig)
	assert.False(t, tlsConfig.InsecureSkipVerify)
	assert.Equal(t, "db.example.com", tlsConfig.ServerName)
	assert.Nil(t, tlsConfig.RootCAs)
}

func TestBuildDSNVerifyFullTLSConfig(t *testing.T) {
	caPath := writeTestCA(t)
	cfg := &ConnectionConfig{
		Username:    "postgres",
		Password:    "secret",
		Host:        "db.example.com:5432",
		Database:    "compose",
		SSLMode:     "verify-full",
		SSLRootCert: caPath,
	}

	sslmode, err := resolveSSLMode(cfg)
	require.NoError(t, err)

	dsn := buildDSN(cfg, sslmode)

	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	assert.Equal(t, caPath, parsed.Query().Get("sslrootcert"))

	tlsConfig := pgdriver.NewConnector(pgdriver.WithDSN(dsn)).Config().TLSConfig
	require.NotNil(t, tlsConfig)
	assert.False(t, tlsConfig.InsecureSkipVerify)
	assert.Equal(t, "db.example.com", tlsConfig.ServerName)
	assert.NotNil(t, tlsConfig.RootCAs)
}

func TestBuildDSNRequireTLSConfig(t *testing.T) {
	cfg := &ConnectionConfig{
		Username: "postgres",
		Host:     "db.example.com:5432",
		Database: "compose",
		SSL:      true,
	}

	sslmode, err := resolveSSLMode(cfg)
	require.NoError(t, err)

	tlsConfig := pgdriver.NewConnector(pgdriver.WithDSN(buildDSN(cfg, sslmode))).Config().TLSConfig
	require.NotNil(t, tlsConfig)
	assert.True(t, tlsConfig.InsecureSkipVerify)
}
