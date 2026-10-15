package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	DataDir      string
	TempDir      string
	DBPath       string
	BridgeDBPath string
}

func (c Config) AttachmentsDir() string {
	return filepath.Join(c.DataDir, "attachments")
}

// ImportsDir holds uploaded exports while they're being imported.
func (c Config) ImportsDir() string {
	return filepath.Join(c.TempDir, "imports")
}

func requireEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("required environment variable %s is not set", name)
	}
	return value, nil
}

func Load() (Config, error) {
	var c Config
	var err error
	if c.DataDir, err = requireEnv("OPENHOST_APP_DATA_DIR"); err != nil {
		return c, err
	}
	if c.TempDir, err = requireEnv("OPENHOST_APP_TEMP_DIR"); err != nil {
		return c, err
	}
	if c.DBPath, err = requireEnv("OPENHOST_SQLITE_MAIN"); err != nil {
		return c, err
	}
	if c.BridgeDBPath, err = requireEnv("OPENHOST_SQLITE_BRIDGE"); err != nil {
		return c, err
	}
	return c, nil
}
