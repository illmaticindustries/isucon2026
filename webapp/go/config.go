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
	LogLevel   string
	ImageFiles *imageFileStore
	// Forwarded marks a listener that serves another region's requests relayed
	// by that region's nginx. Logins, logouts and deletions for that region run
	// in a different process, so this listener must not cache sessions.
	Forwarded bool
}

func ConfigFromEnv() *Config {
	c := &Config{
		Region:     envOrDefault("ISUREAL_REGION", "asia"),
		DBHost:     envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:     envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:     envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword: envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:     envOrDefault("ISUREAL_DB_NAME", "isureal"),
		LogLevel:   envOrDefault("ISUREAL_LOG_LEVEL", "debug"),
	}
	// Every app server serves its own region and keeps a local copy of the
	// immutable image files, so all regions enable the file cache.
	c.ImageFiles = &imageFileStore{root: "/home/isucon/.isureal-image-files"}
	return c
}

func (c *Config) DatabaseDSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.DBUser, c.DBPassword),
		Host:   net.JoinHostPort(c.DBHost, c.DBPort),
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
