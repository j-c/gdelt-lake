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
       gdelt-lake status

  run     process new GDELT batches, compact finished days, and exit
          (0 when the run did its job, 1 when it failed and needs attention, 2 for bad configuration;
          problems with individual GDELT files are retried and reported by status, not exit codes)
  status  print the lake's health and exit 0 (healthy), 3 (data problems: gaps, the lake behind,
          files failing for long) or 1 (the last run failed, or the lake can't be read)

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
	os.Exit(realMain(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, log))
}

// realMain is main without the process: arguments, environment and outputs are passed in, so tests can drive it.
func realMain(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, log *slog.Logger) int {
	if len(args) == 0 || (args[0] != "run" && args[0] != "status") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == "status" {
		return status(ctx, args[1:], getenv, stdout, stderr)
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
		code := ingest.ExitCode(err)
		attrs := []any{"done", sum.Done, "failed", sum.Failed, "missing", sum.Missing, "waiting", sum.Waiting,
			"recovered", sum.Recovered, "compacted", sum.Compacted, "list_errors", sum.ListErrors,
			"seconds", time.Since(started).Round(time.Second).Seconds(), "exit", code}
		if code != 0 {
			log.Error("run failed", append(attrs, "error", err.Error())...)
		} else {
			log.Info("run finished", attrs...)
		}
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

// status prints the lake's health (design 8.4).
func status(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "configuration:", err)
		return 2
	}
	store, err := openStorage(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "storage:", err)
		return ingest.StatusSystem
	}
	h, err := ingest.ReadHealth(ctx, store)
	if err != nil {
		fmt.Fprintln(stderr, "reading the lake's health:", err)
		return ingest.StatusSystem
	}
	code, reasons := ingest.Evaluate(h, time.Now().UTC())
	verdict := map[int]string{ingest.StatusHealthy: "healthy", ingest.StatusData: "data problems",
		ingest.StatusSystem: "needs attention"}[code]
	fmt.Fprintf(stdout, "%s (last run %s, %s)\n", verdict, h.LastRun.Finished.Format(time.RFC3339), h.LastRun.Outcome)
	for _, s := range []string{"en", "tr"} {
		if n := h.Newest[s]; n != nil {
			fmt.Fprintf(stdout, "  %s: newest batch %s\n", s, n.Batch.Format(time.RFC3339))
		}
	}
	for _, r := range reasons {
		fmt.Fprintln(stdout, "  - "+r)
	}
	return code
}

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
