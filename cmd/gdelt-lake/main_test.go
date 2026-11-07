package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j-c/gdelt-lake/internal/source/fakegdelt"
)

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"15m": 15 * time.Minute, "7d": 7 * 24 * time.Hour, "90s": 90 * time.Second} {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"xd", "7 days", ""} {
		if _, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) succeeded, want an error", in)
		}
	}
}

func TestRealMainExitCodes(t *testing.T) {
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	withRoot := func(k string) string {
		if k == "LAKE_LOCAL_ROOT" {
			return t.TempDir()
		}
		return ""
	}
	none := func(string) string { return "" }
	for name, tc := range map[string]struct {
		args   []string
		getenv func(string) string
		code   int
		stderr string
	}{
		"no command":       {nil, withRoot, 2, "usage:"},
		"unknown command":  {[]string{"serve"}, withRoot, 2, "usage:"},
		"bad --every":      {[]string{"run", "--every", "soon"}, withRoot, 2, "--every"},
		"negative --every": {[]string{"run", "--every", "-5m"}, withRoot, 2, "must be positive"},
		"extra arguments":  {[]string{"run", "now"}, withRoot, 2, "unexpected arguments"},
		"missing config":   {[]string{"run"}, none, 2, "LAKE_LOCAL_ROOT"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := realMain(context.Background(), tc.args, tc.getenv, io.Discard, &stderr, quiet)
			if code != tc.code || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("exit %d, stderr %q; want exit %d and stderr containing %q", code, stderr.String(), tc.code, tc.stderr)
			}
		})
	}
}

// One real run against a fake GDELT: the binary's wiring, end to end.
func TestRunOnceAgainstFakeGDELT(t *testing.T) {
	srv := fakegdelt.New(t)
	batch := time.Now().UTC().Truncate(15 * time.Minute)
	for _, s := range []string{"en", "tr"} {
		srv.AddBatch(s, batch, map[string]string{
			"events": "1\t20261030" + strings.Repeat("\t", 59) + "\n",
		}, nil)
	}
	root := t.TempDir()
	env := map[string]string{"LAKE_LOCAL_ROOT": root, "LAKE_GDELT_BASE_URL": srv.BaseURL(), "LAKE_TEMP_DIR": t.TempDir(),
		"LAKE_DUCKDB_MEMORY": "256MB"}
	var stderr, logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	code := realMain(t.Context(), []string{"run"}, func(k string) string { return env[k] }, io.Discard, &stderr, log)
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s\nlogs: %s", code, stderr.String(), logs.String())
	}
	if !strings.Contains(logs.String(), `"msg":"run finished"`) || !strings.Contains(logs.String(), `"done":6`) {
		t.Fatalf("logs: %s", logs.String())
	}
	if _, err := os.Stat(filepath.Join(root, "_state", "lake.json")); err != nil {
		t.Fatal(err)
	}

	// status reads the health file the run wrote: both streams have their current batch, so it's healthy.
	var out bytes.Buffer
	code = realMain(t.Context(), []string{"status"}, func(k string) string { return env[k] }, &out, &stderr, log)
	if code != 0 || !strings.HasPrefix(out.String(), "healthy") {
		t.Fatalf("status exit %d\n%s%s", code, out.String(), stderr.String())
	}
}

func TestStatusOnAnEmptyLake(t *testing.T) {
	env := map[string]string{"LAKE_LOCAL_ROOT": t.TempDir()}
	var stderr bytes.Buffer
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	code := realMain(t.Context(), []string{"status"}, func(k string) string { return env[k] }, io.Discard, &stderr, quiet)
	if code != 1 || !strings.Contains(stderr.String(), "health") {
		t.Fatalf("exit %d, stderr %q; want 1 (can't read the lake's health)", code, stderr.String())
	}
}

func TestLoopStopsCleanly(t *testing.T) {
	srv := fakegdelt.New(t)
	env := map[string]string{"LAKE_LOCAL_ROOT": t.TempDir(), "LAKE_GDELT_BASE_URL": srv.BaseURL(), "LAKE_TEMP_DIR": t.TempDir()}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	start := time.Now()
	code := realMain(ctx, []string{"run", "--every", "100ms"}, func(k string) string { return env[k] }, io.Discard, io.Discard, quiet)
	if code != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("exit %d after %v; want 0 soon after the stop", code, time.Since(start))
	}
}
