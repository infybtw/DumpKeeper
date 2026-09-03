// Package config loads DumpKeeper configuration from environment variables.
package config

import (
	"fmt"
	"os"
)

// Config holds the process-wide settings sourced from the environment.
type Config struct {
	Login      string // AUTH_LOGIN, required
	Password   string // AUTH_PASSWORD, required
	ListenAddr string // LISTEN_ADDR, default ":8080"
	DataDir    string // DATA_DIR, default "/data"
}

// Load reads the environment and fails fast when a required variable is
// missing or empty.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr: ":8080",
		DataDir:    "/data",
	}
	cfg.Login = os.Getenv("AUTH_LOGIN")
	cfg.Password = os.Getenv("AUTH_PASSWORD")
	if cfg.Login == "" {
		return cfg, fmt.Errorf("environment variable AUTH_LOGIN is required")
	}
	if cfg.Password == "" {
		return cfg, fmt.Errorf("environment variable AUTH_PASSWORD is required")
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	return cfg, nil
}
