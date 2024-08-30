package postgres

const DEFAULT_STATEMENT_TIMEOUT = 30 * 1000 // 30 seconds in milliseconds

type ConnectionConfig struct {
	Username         string
	Password         string
	Host             string
	Database         string
	SSL              bool
	Verbose          bool
	StatementTimeout int // Timeout in miliseconds for how long can a statement last before being canceled
}
