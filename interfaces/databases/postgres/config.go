package postgres

type ConnectionConfig struct {
	Username string
	Password string
	Host     string
	Database string
	SSL      bool
	Verbose  bool
}
