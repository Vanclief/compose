package postgres

const (
	DEFAULT_DIAL_TIMEOUT      = 10 * 1000 // 10 seconds in milliseconds
	DEFAULT_READ_TIMEOUT      = 30 * 1000 // 30 seconds in milliseconds
	DEFAULT_WRITE_TIMEOUT     = 20 * 1000 // 20 seconds in milliseconds
	DEFAULT_STATEMENT_TIMEOUT = 30 * 1000 // 30 seconds in milliseconds
)

type ConnectionConfig struct {
	Username         string `mapstructure:"username"`
	Password         string `mapstructure:"password"`
	Host             string `mapstructure:"host"`
	Database         string `mapstructure:"database"`
	SSL              bool   `mapstructure:"ssl"`
	Verbose          bool   `mapstructure:"verbose"`
	DialTimeout      int    `mapstructure:"dialTimeout"`
	ReadTimeout      int    `mapstructure:"readTimeout"`
	WriteTimeout     int    `mapstructure:"writeTimeout"`
	StatementTimeout int    `mapstructure:"statementTimeout"`
}
