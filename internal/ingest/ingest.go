// Package ingest is one run of the ingester: bring the lake up to date with GDELT within the lookback window, then
// compact any day that is finished. Everything it knows comes from the lake's state files, so a run that fails or is
// stopped is completed by the next one.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/j-c/gdelt-lake/internal/convert"
	"github.com/j-c/gdelt-lake/internal/source"
	"github.com/j-c/gdelt-lake/internal/state"
	"github.com/j-c/gdelt-lake/internal/storage"
)

const (
	// GiveUpAfter is how long after its batch time a file may stay unavailable before it's recorded as missing.
	GiveUpAfter = 6 * time.Hour
	// CompactGrace is how long after a UTC day ends before it may be compacted.
	CompactGrace = 2 * time.Hour
)

// Runner holds what a run needs.
type Runner struct {
	Store    storage.Storage
	Source   *source.Client
	Engine   *convert.Engine
	TempDir  string
	Lookback time.Duration
	Log      *slog.Logger
	Now      func() time.Time
}

// Summary counts what a run did.
type Summary struct {
	Done      int // files added to the lake
	Failed    int // files that failed this run (they're retried next run)
	Missing   int // files recorded as missing this run
	Waiting   int // files GDELT hasn't listed yet, still within GiveUpAfter
	Compacted int // days compacted
}

// item is one GDELT file the run may need.
type item struct {
	stream source.Stream
	feed   source.Feed
	batch  time.Time
}

func (it item) key() string  { return state.Key(it.stream, it.feed, it.batch) }
func (it item) date() string { return state.DateOf(it.batch) }

// LakePath is where a batch file lives in the lake: gkg/date=2026-10-31/20261031T001500-en.parquet.
func LakePath(s source.Stream, f source.Feed, batch time.Time) string {
	return string(f) + "/date=" + state.DateOf(batch) + "/" + batch.UTC().Format("20060102T150405") + "-" + string(s) + ".parquet"
}

// DayPath is where a compacted day file lives: gkg/date=2026-10-31/day.parquet.
func DayPath(f source.Feed, date string) string { return string(f) + "/date=" + date + "/day.parquet" }

// Run brings the lake up to date. It returns an error only for problems that stop the run (storage errors, a
// cancelled context); failed files are counted in the summary and retried next time.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	now := r.Now().UTC()
	last := now.Truncate(source.BatchInterval).Add(source.BatchInterval) // GDELT stamps a batch with its end time
	first := last.Add(-r.Lookback).Truncate(source.BatchInterval)

	lake, ok, err := state.LoadLake(ctx, r.Store)
	if err != nil {
		return sum, err
	}
	if !ok { // a new lake starts with the current batch: forward-only (design R4)
		lake = state.Lake{Started: now.Truncate(source.BatchInterval)}
		if err := state.SaveLake(ctx, r.Store, lake); err != nil {
			return sum, err
		}
		r.Log.Info("new lake", "started", lake.Started)
	}
	if first.Before(lake.Started) {
		first = lake.Started
	}

	days := map[string]*state.Day{}
	var order []string
	for d := first.Truncate(24 * time.Hour); !d.After(last); d = d.Add(24 * time.Hour) {
		date := state.DateOf(d)
		day, err := state.Load(ctx, r.Store, date)
		if err != nil {
			return sum, err
		}
		days[date] = day
		order = append(order, date)
	}

	var pending []item
	for b := first; !b.After(last); b = b.Add(source.BatchInterval) {
		day := days[state.DateOf(b)]
		if day.CompactedAt != nil {
			continue
		}
		for _, s := range source.Streams {
			for _, f := range source.Feeds {
				it := item{s, f, b}
				if !day.Files[it.key()].Settled() {
					pending = append(pending, it)
				}
			}
		}
	}

	listed, listErr := r.list(ctx, pending)
	if ctx.Err() != nil {
		return sum, ctx.Err()
	}
	if listErr != nil {
		r.Log.Error("reading GDELT's file lists", "error", listErr)
		sum.Failed++
	}
	for _, it := range pending {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if listErr != nil {
			break
		}
		day := days[it.date()]
		old := now.Sub(it.batch) > GiveUpAfter
		f, ok := listed[source.FileName(it.stream, it.feed, it.batch)]
		if !ok {
			if !old {
				sum.Waiting++
				continue
			}
			r.settle(day, it, state.Missing, "not in GDELT's file list")
			sum.Missing++
		} else {
			st, err := r.add(ctx, f)
			if err != nil && ctx.Err() != nil {
				return sum, ctx.Err()
			}
			prev := day.Files[it.key()]
			switch {
			case err == nil:
				day.Files[it.key()] = st
				sum.Done++
			case old:
				r.settle(day, it, state.Missing, err.Error())
				sum.Missing++
			default:
				attempts := 1
				if prev != nil {
					attempts = prev.Attempts + 1
				}
				day.Files[it.key()] = &state.File{Status: state.Failed, Attempts: attempts, Error: err.Error(), Updated: r.Now().UTC()}
				sum.Failed++
				r.Log.Warn("file failed; retrying next run", "file", f.Name, "attempts", attempts, "error", err)
			}
		}
		if err := state.Save(ctx, r.Store, day); err != nil {
			return sum, err
		}
	}

	for _, date := range order {
		n, err := r.compact(ctx, days[date], now, lake.Started)
		if err != nil {
			return sum, fmt.Errorf("compacting %s: %w", date, err)
		}
		sum.Compacted += n
	}
	return sum, nil
}

func (r *Runner) settle(day *state.Day, it item, st state.Status, why string) {
	prev := day.Files[it.key()]
	attempts := 0
	if prev != nil {
		attempts = prev.Attempts
	}
	day.Files[it.key()] = &state.File{Status: st, Attempts: attempts, Error: why, Updated: r.Now().UTC()}
	r.Log.Warn("file recorded as missing", "file", source.FileName(it.stream, it.feed, it.batch), "reason", why)
}

// list returns GDELT's listed files by name, reading only as much of the lists as the pending items need.
func (r *Runner) list(ctx context.Context, pending []item) (map[string]source.File, error) {
	out := map[string]source.File{}
	for _, s := range source.Streams {
		var oldest time.Time
		for _, it := range pending {
			if it.stream == s && (oldest.IsZero() || it.batch.Before(oldest)) {
				oldest = it.batch
			}
		}
		if oldest.IsZero() {
			continue
		}
		files, err := r.Source.Latest(ctx, s)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 || oldest.Before(files[0].Batch) {
			// Something older than the latest batch is still pending: read the end of the master list.
			if files, err = r.Source.Since(ctx, s, oldest); err != nil {
				return nil, err
			}
		}
		for _, f := range files {
			out[f.Name] = f
		}
	}
	return out, nil
}

// add downloads, converts and stores one file.
func (r *Runner) add(ctx context.Context, f source.File) (*state.File, error) {
	tmp, err := os.MkdirTemp(r.TempDir, "batch-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	zipPath, err := r.Source.Download(ctx, f, tmp)
	if err != nil {
		return nil, err
	}
	csv, err := source.Unzip(zipPath, tmp)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(zipPath)
	out := filepath.Join(tmp, "out.parquet")
	res, err := r.Engine.Batch(ctx, f.Feed, f.Stream == source.Translingual, csv, out)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(csv)
	size, err := r.put(ctx, out, LakePath(f.Stream, f.Feed, f.Batch))
	if err != nil {
		return nil, err
	}
	if res.Rejected() > 0 {
		r.Log.Warn("records DuckDB couldn't read", "file", f.Name, "rejected", res.Rejected())
	}
	r.Log.Info("added", "file", f.Name, "rows", res.Rows, "bytes", size)
	return &state.File{Status: state.Done, Updated: r.Now().UTC(), Records: res.Records, Rows: res.Rows,
		Rejected: res.Rejected(), ContinuationLines: res.ContinuationLines, InvalidUTF8Lines: res.InvalidUTF8Lines,
		Bytes: size}, nil
}

// compact merges a finished day's batch files into one file per feed. It returns 1 if it compacted the day.
func (r *Runner) compact(ctx context.Context, day *state.Day, now, started time.Time) (int, error) {
	if day.CompactedAt != nil {
		return 0, r.removeBatchFiles(ctx, day) // finishes a run that stopped between recording and deleting
	}
	start, err := time.Parse("2006-01-02", day.Date)
	if err != nil {
		return 0, err
	}
	if now.Before(start.Add(24*time.Hour + CompactGrace)) {
		return 0, nil
	}
	from := start
	if started.After(from) {
		from = started // slots before the lake started are never collected
	}
	for b := from; b.Before(start.Add(24 * time.Hour)); b = b.Add(source.BatchInterval) {
		for _, s := range source.Streams {
			for _, f := range source.Feeds {
				if !day.Files[state.Key(s, f, b)].Settled() {
					return 0, nil
				}
			}
		}
	}
	tmp, err := os.MkdirTemp(r.TempDir, "compact-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	compacted := map[source.Feed]*state.Compacted{}
	for _, f := range source.Feeds {
		var want int64
		for key, st := range day.Files {
			if st.Status == state.Done && strings.HasSuffix(key, "-"+string(f)) {
				want += st.Rows
			}
		}
		inputs, err := r.batchFiles(ctx, f, day.Date)
		if err != nil {
			return 0, err
		}
		if len(inputs) == 0 {
			compacted[f] = &state.Compacted{}
			continue
		}
		var local []string
		for i, p := range inputs {
			dst := filepath.Join(tmp, fmt.Sprintf("%s-%04d.parquet", f, i))
			if err := r.get(ctx, p, dst); err != nil {
				return 0, err
			}
			local = append(local, dst)
		}
		out := filepath.Join(tmp, string(f)+"-day.parquet")
		rows, err := r.Engine.Compact(ctx, local, out)
		if err != nil {
			return 0, err
		}
		if rows != want {
			return 0, fmt.Errorf("%s: the day file has %d rows, its batches %d", f, rows, want)
		}
		size, err := r.put(ctx, out, DayPath(f, day.Date))
		if err != nil {
			return 0, err
		}
		compacted[f] = &state.Compacted{Rows: rows, Bytes: size}
		for _, l := range local {
			_ = os.Remove(l)
		}
	}
	at := r.Now().UTC()
	day.Compacted, day.CompactedAt = compacted, &at
	if err := state.Save(ctx, r.Store, day); err != nil {
		return 0, err
	}
	r.Log.Info("compacted", "date", day.Date)
	return 1, r.removeBatchFiles(ctx, day)
}

// batchFiles lists a day's batch files for one feed (everything but day.parquet).
func (r *Runner) batchFiles(ctx context.Context, f source.Feed, date string) ([]string, error) {
	all, err := r.Store.List(ctx, string(f)+"/date="+date+"/")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(p string) bool { return p == DayPath(f, date) }), nil
}

func (r *Runner) removeBatchFiles(ctx context.Context, day *state.Day) error {
	for _, f := range source.Feeds {
		files, err := r.batchFiles(ctx, f, day.Date)
		if err != nil {
			return err
		}
		for _, p := range files {
			if err := r.Store.Delete(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// put copies a local file into the lake and returns its size.
func (r *Runner) put(ctx context.Context, local, lakePath string) (int64, error) {
	fh, err := os.Open(local)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), r.Store.Write(ctx, lakePath, fh)
}

// get copies a lake file to a local path.
func (r *Runner) get(ctx context.Context, lakePath, local string) error {
	in, err := r.Store.Open(ctx, lakePath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(local)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ExitCode maps a run's outcome to the process exit code: 0 when everything is accounted for, 1 otherwise.
func ExitCode(sum Summary, err error) int {
	if errors.Is(err, context.Canceled) {
		return 0 // stopped on request; the next run carries on
	}
	if err != nil {
		return 1
	}
	if sum.Failed > 0 {
		return 1
	}
	return 0
}
