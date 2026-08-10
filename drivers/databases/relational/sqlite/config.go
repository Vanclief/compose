package sqlite

const (
	DEFAULT_BUSY_TIMEOUT = 10 * 1000 // 10 seconds in milliseconds
)

type ConnectionConfig struct {
	Path        string `mapstructure:"path"`
	BusyTimeout int    `mapstructure:"busyTimeout"`
	// MaxOpenConns caps the connection pool. SQLite allows a single writer
	// at a time, so setting this to 1 serializes access in the pool instead
	// of relying on busy timeout retries. 0 leaves the pool unlimited.
	MaxOpenConns int  `mapstructure:"maxOpenConns"`
	Verbose      bool `mapstructure:"verbose"`
}
