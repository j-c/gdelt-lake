// Package config reads gdelt-lake's settings from environment variables. There are no config files: deployments set
// the variables, and tests pass a map.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// StorageKind names a storage backend.
type StorageKind string

const (
	// StorageLocal keeps the lake in a folder on a local or mounted volume.
	StorageLocal StorageKind = "local"
	// StorageAzure keeps the lake in an Azure Blob Storage container.
	StorageAzure StorageKind = "azure"
)

// DefaultGDELTBaseURL is where GDELT 2.0 publishes its file lists and 15-minute batches.
const DefaultGDELTBaseURL = "https://data.gdeltproject.org/gdeltv2/"

// Config is everything the ingester needs besides its command-line flags.
type Config struct {
	Storage      StorageKind // LAKE_STORAGE: which backend holds the lake (default "local")
	LocalRoot    string      // LAKE_LOCAL_ROOT: the lake's folder, for the local backend
	DuckDBMemory string      // LAKE_DUCKDB_MEMORY: DuckDB's memory cap, e.g. "1GB" (default "1GB")
	GDELTBaseURL string      // LAKE_GDELT_BASE_URL: GDELT's file server (tests point this at a fake one)
	TempDir      string      // LAKE_TEMP_DIR: scratch space for downloads and Parquet before upload

	// Azure Blob Storage, for LAKE_STORAGE=azure: LAKE_AZURE_ACCOUNT_URL (credentials from the standard Azure chain:
	// managed identity, or `az login`) or LAKE_AZURE_CONNECTION_STRING (a SAS, or Azurite); LAKE_AZURE_CONTAINER
	// (default "gdelt").
	AzureAccountURL       string
	AzureConnectionString string
	AzureContainer        string

	// LAKE_BACKFILL: how much history to hold (design 8.5), either a duration ("90d") or a date ("2025-01-01").
	// At most one of these is set; neither means no backfill.
	BackfillFor  time.Duration
	BackfillFrom time.Time
	// LAKE_BACKFILL_BUDGET: how long a run may spend on backfill (default 10m; 0 pauses backfill).
	BackfillBudget time.Duration
}

// GDELT2Start is GDELT 2.0's first batch; there's nothing earlier in this format.
var GDELT2Start = time.Date(2015, 2, 18, 22, 45, 0, 0, time.UTC)

// BackfillTarget is the oldest batch time the lake should hold at time now, or zero for no backfill.
func (c Config) BackfillTarget(now time.Time) time.Time {
	var t time.Time
	switch {
	case !c.BackfillFrom.IsZero():
		t = c.BackfillFrom
	case c.BackfillFor > 0:
		t = now.UTC().Add(-c.BackfillFor).Truncate(24 * time.Hour)
	default:
		return time.Time{}
	}
	if t.Before(GDELT2Start) {
		t = GDELT2Start
	}
	return t
}

// Load reads the configuration through getenv (os.Getenv in production) and checks it.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Storage:      StorageKind(or(getenv("LAKE_STORAGE"), string(StorageLocal))),
		LocalRoot:    getenv("LAKE_LOCAL_ROOT"),
		DuckDBMemory: or(getenv("LAKE_DUCKDB_MEMORY"), "1GB"),
		GDELTBaseURL: or(getenv("LAKE_GDELT_BASE_URL"), DefaultGDELTBaseURL),
		TempDir:      or(getenv("LAKE_TEMP_DIR"), os.TempDir()),

		AzureAccountURL:       getenv("LAKE_AZURE_ACCOUNT_URL"),
		AzureConnectionString: getenv("LAKE_AZURE_CONNECTION_STRING"),
		AzureContainer:        or(getenv("LAKE_AZURE_CONTAINER"), "gdelt"),
	}
	if !strings.HasSuffix(c.GDELTBaseURL, "/") {
		c.GDELTBaseURL += "/"
	}
	if v := getenv("LAKE_BACKFILL"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			c.BackfillFrom = t
		} else if d, err := ParseDuration(v); err == nil && d > 0 {
			c.BackfillFor = d
		} else {
			return Config{}, fmt.Errorf("LAKE_BACKFILL %q: want a duration such as 90d or a date such as 2025-01-01", v)
		}
	}
	c.BackfillBudget = 10 * time.Minute
	if v := getenv("LAKE_BACKFILL_BUDGET"); v != "" {
		d, err := ParseDuration(v)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("LAKE_BACKFILL_BUDGET %q: want a duration such as 10m (0 pauses backfill)", v)
		}
		c.BackfillBudget = d
	}
	switch c.Storage {
	case StorageLocal:
		if c.LocalRoot == "" {
			return Config{}, fmt.Errorf("LAKE_LOCAL_ROOT is required when LAKE_STORAGE is %q", StorageLocal)
		}
	case StorageAzure:
		if (c.AzureAccountURL == "") == (c.AzureConnectionString == "") {
			return Config{}, fmt.Errorf("LAKE_STORAGE %q needs one of LAKE_AZURE_ACCOUNT_URL or LAKE_AZURE_CONNECTION_STRING",
				StorageAzure)
		}
	default:
		return Config{}, fmt.Errorf("LAKE_STORAGE %q is not supported (supported: %q, %q)", c.Storage, StorageLocal,
			StorageAzure)
	}
	return c, nil
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// ParseDuration accepts Go durations plus whole days ("7d"), which Go's own parser doesn't.
func ParseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
