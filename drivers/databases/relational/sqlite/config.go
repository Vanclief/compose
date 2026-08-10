package sqlite

const (
	DEFAULT_BUSY_TIMEOUT = 10 * 1000 // 10 seconds in milliseconds
)

type ConnectionConfig struct {
	Path        string `mapstructure:"path"`
	BusyTimeout int    `mapstructure:"busyTimeout"`
	Verbose     bool   `mapstructure:"verbose"`
}
