package postgres

const DEFAULT_DIAL_TIMEOUT = 10 * 1000      // 10 seconds in milliseconds
const DEFAULT_READ_TIMEOUT = 30 * 1000      // 30 seconds in milliseconds
const DEFAULT_WRITE_TIMEOUT = 20 * 1000     // 20 seconds in milliseconds
const DEFAULT_STATEMENT_TIMEOUT = 30 * 1000 // 30 seconds in milliseconds

type ConnectionConfig struct {
	Username         string `mapstructure:"username"`
	Password         string `mapstructure:"password"`
	Host             string `mapstructure:"host"`
	Database         string `mapstructure:"database"`
	SSL              bool   `mapstructure:"ssl"`
	Verbose          bool   `mapstructure:"verbose"`
	DialTimeout      int    `mapstructure:"dial_timeout"`
	ReadTimeout      int    `mapstructure:"read_timeout"`
	WriteTimeout     int    `mapstructure:"write_timeout"`
	StatementTimeout int    `mapstructure:"statement_timeout"` // Timeout in miliseconds for how long can a statement last before being canceled
}
