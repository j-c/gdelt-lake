// Package config reads gdelt-lake's settings from environment variables. There are no config files: deployments set
// the variables, and tests pass a map.
package config

import (
	"fmt"
	"os"
	"strings"
)

// StorageKind names a storage backend.
type StorageKind string

const (
	// StorageLocal keeps the lake in a folder on a local or mounted volume.
	StorageLocal StorageKind = "local"
)

// DefaultGDELTBaseURL is where GDELT 2.0 publishes its file lists and 15-minute batches.
const DefaultGDELTBaseURL = "http://data.gdeltproject.org/gdeltv2/"

// Config is everything the ingester needs besides its command-line flags.
type Config struct {
	Storage      StorageKind // LAKE_STORAGE: which backend holds the lake (default "local")
	LocalRoot    string      // LAKE_LOCAL_ROOT: the lake's folder, for the local backend
	DuckDBMemory string      // LAKE_DUCKDB_MEMORY: DuckDB's memory cap, e.g. "1GB" (default "1GB")
	GDELTBaseURL string      // LAKE_GDELT_BASE_URL: GDELT's file server (tests point this at a fake one)
	TempDir      string      // LAKE_TEMP_DIR: scratch space for downloads and Parquet before upload
}

// Load reads the configuration through getenv (os.Getenv in production) and checks it.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Storage:      StorageKind(or(getenv("LAKE_STORAGE"), string(StorageLocal))),
		LocalRoot:    getenv("LAKE_LOCAL_ROOT"),
		DuckDBMemory: or(getenv("LAKE_DUCKDB_MEMORY"), "1GB"),
		GDELTBaseURL: or(getenv("LAKE_GDELT_BASE_URL"), DefaultGDELTBaseURL),
		TempDir:      or(getenv("LAKE_TEMP_DIR"), os.TempDir()),
	}
	if !strings.HasSuffix(c.GDELTBaseURL, "/") {
		c.GDELTBaseURL += "/"
	}
	switch c.Storage {
	case StorageLocal:
		if c.LocalRoot == "" {
			return Config{}, fmt.Errorf("LAKE_LOCAL_ROOT is required when LAKE_STORAGE is %q", StorageLocal)
		}
	default:
		return Config{}, fmt.Errorf("LAKE_STORAGE %q is not supported (supported: %q)", c.Storage, StorageLocal)
	}
	return c, nil
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
