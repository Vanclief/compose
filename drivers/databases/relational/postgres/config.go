package postgres

const (
	DEFAULT_DIAL_TIMEOUT      = 10 * 1000 // 10 seconds in milliseconds
	DEFAULT_READ_TIMEOUT      = 30 * 1000 // 30 seconds in milliseconds
	DEFAULT_WRITE_TIMEOUT     = 20 * 1000 // 20 seconds in milliseconds
	DEFAULT_STATEMENT_TIMEOUT = 30 * 1000 // 30 seconds in milliseconds
)

type ConnectionConfig struct {
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	Host     string `mapstructure:"host"`
	Database string `mapstructure:"database"`
	// SSL enables TLS with sslmode=require: traffic is encrypted but the server
	// certificate is not verified. Ignored when SSLMode is set.
	SSL bool `mapstructure:"ssl"`
	// SSLMode overrides SSL when set. Allowed: "disable", "require", "verify-ca"
	// and "verify-full". Leave empty to keep the SSL behavior.
	SSLMode string `mapstructure:"sslMode"`
	// SSLRootCert is the path to the CA PEM used to verify the server
	// certificate. Only used by verify-ca and verify-full; when empty they
	// verify against the system CA pool.
	SSLRootCert      string `mapstructure:"sslRootCert"`
	Verbose          bool   `mapstructure:"verbose"`
	DialTimeout      int    `mapstructure:"dialTimeout"`
	ReadTimeout      int    `mapstructure:"readTimeout"`
	WriteTimeout     int    `mapstructure:"writeTimeout"`
	StatementTimeout int    `mapstructure:"statementTimeout"`
	// MaxOpenConns caps the connection pool. 0 leaves it unlimited (database/sql default).
	// Set it below the server's max_connections with headroom for other clients.
	MaxOpenConns int `mapstructure:"maxOpenConns"`
	// MaxIdleConns caps idle connections kept in the pool. 0 keeps the database/sql default.
	MaxIdleConns int `mapstructure:"maxIdleConns"`
}
