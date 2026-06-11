package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Server  ServerConfig
	Storage StorageConfig
	Search  BackendConfig
	Vector  BackendConfig
	Auth    AuthConfig
}

type ServerConfig struct {
	Host    string
	Port    int
	DataDir string
}

type StorageConfig struct {
	DSN string
}

type BackendConfig struct {
	DSN string
}

type AuthConfig struct {
	Mode  string
	Token string
}

func Default() Config {
	dataDir := ".docgraph"
	dbPath := filepath.ToSlash(filepath.Join(dataDir, "docgraph.db"))
	return Config{
		Server: ServerConfig{
			Host:    "127.0.0.1",
			Port:    8787,
			DataDir: dataDir,
		},
		Storage: StorageConfig{DSN: "sqlite://" + dbPath},
		Search:  BackendConfig{DSN: "sqlite://" + dbPath},
		Vector:  BackendConfig{DSN: "none://"},
		Auth:    AuthConfig{Mode: "none"},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	section := ""
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, ":") && !strings.Contains(line, " ") {
			section = strings.TrimSuffix(line, ":")
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Config{}, fmt.Errorf("%s:%d: expected key: value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if err := apply(&cfg, section, key, value); err != nil {
			return Config{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, err
	}

	if cfg.Storage.DSN == "" {
		return Config{}, fmt.Errorf("storage.dsn is required")
	}
	if cfg.Auth.Mode == "" {
		cfg.Auth.Mode = "none"
	}
	if cfg.Auth.Mode != "none" && cfg.Auth.Mode != "token" {
		return Config{}, fmt.Errorf("auth.mode must be none or token")
	}
	if cfg.Auth.Mode == "token" && cfg.Auth.Token == "" {
		return Config{}, fmt.Errorf("auth.token is required when auth.mode is token")
	}
	return cfg, nil
}

func stripComment(line string) string {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		return line[:i]
	}
	return line
}

func apply(cfg *Config, section, key, value string) error {
	switch section {
	case "server":
		switch key {
		case "host":
			cfg.Server.Host = value
		case "port":
			port, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid server.port")
			}
			cfg.Server.Port = port
		case "data_dir":
			cfg.Server.DataDir = value
		default:
			return fmt.Errorf("unknown server key %q", key)
		}
	case "storage":
		if key != "dsn" {
			return fmt.Errorf("unknown storage key %q", key)
		}
		cfg.Storage.DSN = value
	case "search":
		if key != "dsn" {
			return fmt.Errorf("unknown search key %q", key)
		}
		cfg.Search.DSN = value
	case "vector":
		if key != "dsn" {
			return fmt.Errorf("unknown vector key %q", key)
		}
		cfg.Vector.DSN = value
	case "auth":
		switch key {
		case "mode":
			cfg.Auth.Mode = value
		case "token":
			cfg.Auth.Token = value
		default:
			return fmt.Errorf("unknown auth key %q", key)
		}
	case "":
		return fmt.Errorf("top-level key %q is not supported", key)
	default:
		return fmt.Errorf("unknown section %q", section)
	}
	return nil
}
