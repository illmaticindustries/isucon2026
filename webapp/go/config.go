package isureal

import (
	"net"
	"net/url"
	"os"
)

type Config struct {
	Region     string
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	// DBReplicaHost optionally points read-only queries at a local streaming
	// replica. Empty means the replica pool falls back to the primary.
	DBReplicaHost string
	DBReplicaPort string
	LogLevel      string
	ImageFiles    *imageFileStore
	// Forwarded marks a listener that serves another region's requests relayed
	// by that region's nginx. Logins, logouts and deletions for that region run
	// in a different process, so this listener must not cache sessions.
	Forwarded bool
}

func ConfigFromEnv() *Config {
	c := &Config{
		Region:        envOrDefault("ISUREAL_REGION", "asia"),
		DBHost:        envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:        envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:        envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword:    envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:        envOrDefault("ISUREAL_DB_NAME", "isureal"),
		DBReplicaHost: envOrDefault("ISUREAL_DB_REPLICA_HOST", ""),
		DBReplicaPort: envOrDefault("ISUREAL_DB_REPLICA_PORT", envOrDefault("ISUREAL_DB_PORT", "5432")),
		LogLevel:      envOrDefault("ISUREAL_LOG_LEVEL", "info"),
	}
	// Every app server serves its own region and keeps a local copy of the
	// immutable image files, so all regions enable the file cache.
	c.ImageFiles = &imageFileStore{root: "/home/isucon/.isureal-image-files"}
	return c
}

func (c *Config) DatabaseDSN() string {
	return c.dsn(c.DBHost, c.DBPort)
}

// ReplicaDatabaseDSN returns the DSN for the read replica, or the primary DSN
// when no replica host is configured.
func (c *Config) ReplicaDatabaseDSN() string {
	if c.DBReplicaHost == "" {
		return c.DatabaseDSN()
	}
	return c.dsn(c.DBReplicaHost, c.DBReplicaPort)
}

func (c *Config) dsn(host, port string) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.DBUser, c.DBPassword),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + c.DBName,
	}
	return u.String()
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
