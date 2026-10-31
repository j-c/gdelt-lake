// Command gdelt-lake keeps a copy of GDELT 2.0's raw 15-minute feeds as Parquet files. See docs/design.md.
//
//	gdelt-lake run [--every 15m] [--lookback 7d]
//
// Settings come from environment variables (internal/config).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/j-c/gdelt-lake/internal/config"
	"github.com/j-c/gdelt-lake/internal/convert"
	"github.com/j-c/gdelt-lake/internal/ingest"
	"github.com/j-c/gdelt-lake/internal/source"
	"github.com/j-c/gdelt-lake/internal/storage"
)

const usage = `usage: gdelt-lake run [--every DURATION] [--lookback DURATION]

  run   process new GDELT batches, compact finished days, and exit
        (0 when every batch is accounted for, 1 when anything failed)

Durations: Go durations (90s, 15m, 2h) or whole days (7d).
Settings come from environment variables; see README.md.
`

// runFlags are the command-line options of "run".
type runFlags struct {
	every    time.Duration // 0 = run once and exit
	lookback time.Duration
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(realMain(ctx, os.Args[1:], os.Getenv, os.Stderr, log))
}

// realMain is main without the process: arguments, environment and outputs are passed in, so tests can drive it.
func realMain(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, log *slog.Logger) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	f, err := parseRunFlags(args[1:], stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprint(stderr, usage)
		return 2
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "configuration:", err)
		return 2
	}
	log.Info("starting", "storage", cfg.Storage, "every", f.every.String(), "lookback", f.lookback.String())

	store, err := openStorage(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "storage:", err)
		return 1
	}
	engine, err := convert.Open(cfg.DuckDBMemory, cfg.TempDir)
	if err != nil {
		fmt.Fprintln(stderr, "duckdb:", err)
		return 1
	}
	defer engine.Close()
	runner := &ingest.Runner{
		Store:    store,
		Source:   source.NewClient(cfg.GDELTBaseURL, userAgent),
		Engine:   engine,
		TempDir:  cfg.TempDir,
		Lookback: f.lookback,
		Log:      log,
		Now:      time.Now,
	}
	runOnce := func() int {
		started := time.Now()
		sum, err := runner.Run(ctx)
		code := ingest.ExitCode(sum, err)
		attrs := []any{"done", sum.Done, "failed", sum.Failed, "missing", sum.Missing, "waiting", sum.Waiting,
			"compacted", sum.Compacted, "seconds", time.Since(started).Round(time.Second).Seconds(), "exit", code}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		log.Info("run finished", attrs...)
		return code
	}
	if f.every == 0 {
		return runOnce()
	}
	tick := time.NewTicker(f.every)
	defer tick.Stop()
	for {
		runOnce() // in loop mode a failed run is logged and retried on the next tick
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return 0
		case <-tick.C:
		}
	}
}

// userAgent identifies the ingester to GDELT's file server.
const userAgent = "gdelt-lake (+https://github.com/j-c/gdelt-lake)"

func openStorage(cfg config.Config) (storage.Storage, error) {
	switch cfg.Storage {
	case config.StorageLocal:
		return storage.NewLocal(cfg.LocalRoot)
	}
	return nil, fmt.Errorf("storage %q is not supported", cfg.Storage)
}

func parseRunFlags(args []string, stderr io.Writer) (runFlags, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	every := fs.String("every", "", "repeat on this interval until stopped (default: run once)")
	lookback := fs.String("lookback", "7d", "how far back to look for batches not yet in the lake")
	if err := fs.Parse(args); err != nil {
		return runFlags{}, err
	}
	if fs.NArg() > 0 {
		return runFlags{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	var f runFlags
	var err error
	if *every != "" {
		if f.every, err = parseDuration(*every); err != nil {
			return runFlags{}, fmt.Errorf("--every: %w", err)
		}
		if f.every <= 0 {
			return runFlags{}, errors.New("--every must be positive")
		}
	}
	if f.lookback, err = parseDuration(*lookback); err != nil {
		return runFlags{}, fmt.Errorf("--lookback: %w", err)
	}
	if f.lookback <= 0 {
		return runFlags{}, errors.New("--lookback must be positive")
	}
	return f, nil
}

// parseDuration accepts Go durations plus whole days ("7d"), which Go's own parser doesn't.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
