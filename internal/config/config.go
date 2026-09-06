// Package config loads DumpKeeper configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds the process-wide settings sourced from the environment.
type Config struct {
	Login      string // AUTH_LOGIN, required
	Password   string // AUTH_PASSWORD, required
	ListenAddr string // LISTEN_ADDR, default ":8080"
	DataDir    string // DATA_DIR, default "/data"
	BasePath   string // BASE_PATH, default "" (URL prefix the whole UI lives under)
}

// Load reads the environment and fails fast when a required variable is
// missing or empty.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr: ":8080",
		DataDir:    "/data",
		BasePath:   normalizeBasePath(os.Getenv("BASE_PATH")),
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

// normalizeBasePath turns BASE_PATH into a leading-slash path prefix without
// a trailing slash ("dumpkeeper/" -> "/dumpkeeper"; "/" and "" both mean
// "no prefix"). Empty is the default: the panel lives at the root.
func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
