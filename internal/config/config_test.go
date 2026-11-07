package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"LAKE_LOCAL_ROOT": "/lake", "LAKE_TEMP_DIR": "/scratch"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Storage: StorageLocal, LocalRoot: "/lake", DuckDBMemory: "1GB",
		GDELTBaseURL: DefaultGDELTBaseURL, TempDir: "/scratch", BackfillBudget: 10 * time.Minute}
	if c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestLoadAddsTrailingSlashToBaseURL(t *testing.T) {
	c, err := Load(env(map[string]string{"LAKE_LOCAL_ROOT": "/lake", "LAKE_GDELT_BASE_URL": "http://fake:8080/gdeltv2"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.GDELTBaseURL != "http://fake:8080/gdeltv2/" {
		t.Fatalf("base URL %q", c.GDELTBaseURL)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"local without root": {map[string]string{}, "LAKE_LOCAL_ROOT is required"},
		"unknown backend":    {map[string]string{"LAKE_STORAGE": "floppy"}, `"floppy" is not supported`},
		"bad backfill":       {map[string]string{"LAKE_LOCAL_ROOT": "/l", "LAKE_BACKFILL": "lots"}, "LAKE_BACKFILL"},
		"bad budget":         {map[string]string{"LAKE_LOCAL_ROOT": "/l", "LAKE_BACKFILL_BUDGET": "-1m"}, "LAKE_BACKFILL_BUDGET"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBackfill(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 30, 0, 0, time.UTC)
	for v, want := range map[string]time.Time{
		"":           {},
		"90d":        time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
		"2025-01-01": time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		"2010-01-01": GDELT2Start, // nothing earlier exists in GDELT 2.0's formats
		"36h":        time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC),
	} {
		c, err := Load(env(map[string]string{"LAKE_LOCAL_ROOT": "/l", "LAKE_BACKFILL": v, "LAKE_BACKFILL_BUDGET": "0"}))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if got := c.BackfillTarget(now); !got.Equal(want) {
			t.Errorf("LAKE_BACKFILL=%q: target %v, want %v", v, got, want)
		}
		if c.BackfillBudget != 0 {
			t.Errorf("budget %v, want 0", c.BackfillBudget)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"15m": 15 * time.Minute, "7d": 7 * 24 * time.Hour, "90s": 90 * time.Second} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"xd", "7 days", ""} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded, want an error", in)
		}
	}
}
