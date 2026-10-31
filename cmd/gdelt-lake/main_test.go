package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
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
		"valid":            {[]string{"run", "--every", "15m", "--lookback", "2d"}, withRoot, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := realMain(context.Background(), tc.args, tc.getenv, &stderr, quiet)
			if code != tc.code || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("exit %d, stderr %q; want exit %d and stderr containing %q", code, stderr.String(), tc.code, tc.stderr)
			}
		})
	}
}
