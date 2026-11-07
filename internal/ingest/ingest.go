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

	// Backfill (design 8.5). BackfillTarget gives the oldest batch the lake should hold at a time (zero: none).
	BackfillTarget func(now time.Time) time.Time
	BackfillBudget time.Duration // the most a run spends on backfill; 0 pauses it
	BackfillPause  time.Duration // between backfill downloads
	wall           func() time.Time
}

// Summary counts what a run did.
type Summary struct {
	Done      int // files added to the lake
	Failed    int // files that failed this run (they're retried next run)
	Missing   int // files recorded as missing this run
	Waiting   int // files GDELT hasn't listed yet, still within GiveUpAfter
	Recovered int // files recorded as missing earlier that arrived this run
	Compacted int // days compacted
	// ListErrors counts runs where GDELT's file lists couldn't be read (this run: 0 or 1).
	ListErrors int
	// Backfill: files added to and recorded missing in history, and history days completed, this run.
	BackfillDone, BackfillMissing, BackfillDays int
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

// Run brings the lake up to date and writes _state/health.json. It returns an error only for problems that stop the
// run and need attention (storage, DuckDB, a cancelled context). Problems with individual files are soft: they're
// counted in the summary, retried on later runs and reported in health.json (design 8.1, A18).
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	started := r.Now().UTC()
	sum, days, err := r.run(ctx)
	if ctx.Err() != nil {
		return sum, err // stopped on request; health is written by the next run
	}
	if herr := r.writeHealth(ctx, started, sum, days, err); herr != nil && err == nil {
		err = fmt.Errorf("writing health: %w", herr)
	}
	return sum, err
}

func (r *Runner) run(ctx context.Context) (Summary, map[string]*state.Day, error) {
	var sum Summary
	now := r.Now().UTC()
	last := now.Truncate(source.BatchInterval).Add(source.BatchInterval) // GDELT stamps a batch with its end time
	first := last.Add(-r.Lookback).Truncate(source.BatchInterval)

	lake, ok, err := state.LoadLake(ctx, r.Store)
	if err != nil {
		return sum, nil, err
	}
	if !ok { // a new lake starts with the current batch
		lake = state.Lake{Started: now.Truncate(source.BatchInterval)}
		if err := state.SaveLake(ctx, r.Store, lake); err != nil {
			return sum, nil, err
		}
		r.Log.Info("new lake", "started", lake.Started)
	}
	if first.Before(lake.First()) { // backfilled history in the window is re-checked and compacted like live data
		first = lake.First()
	}

	days := map[string]*state.Day{}
	var order []string
	for d := first.Truncate(24 * time.Hour); !d.After(last); d = d.Add(24 * time.Hour) {
		date := state.DateOf(d)
		day, err := state.Load(ctx, r.Store, date)
		if err != nil {
			return sum, nil, err
		}
		days[date] = day
		order = append(order, date)
	}

	// Everything in the window not yet in the lake, including files recorded as missing: GDELT may still publish
	// them, and a late file re-opens its day (design 8.3).
	var pending []item
	for b := first; !b.After(last); b = b.Add(source.BatchInterval) {
		day := days[state.DateOf(b)]
		for _, s := range source.Streams {
			for _, f := range source.Feeds {
				it := item{s, f, b}
				if st := day.Files[it.key()]; st == nil || st.Status != state.Done {
					pending = append(pending, it)
				}
			}
		}
	}

	listed, listErr := r.list(ctx, pending)
	if ctx.Err() != nil {
		return sum, days, ctx.Err()
	}
	if listErr != nil {
		r.Log.Warn("couldn't read GDELT's file lists; trying again next run", "error", listErr)
		sum.ListErrors++
	}
	for _, it := range pending {
		if ctx.Err() != nil {
			return sum, days, ctx.Err()
		}
		if listErr != nil {
			break
		}
		day := days[it.date()]
		prev := day.Files[it.key()]
		wasMissing := prev != nil && prev.Status == state.Missing
		old := now.Sub(it.batch) > GiveUpAfter
		f, ok := listed[source.FileName(it.stream, it.feed, it.batch)]
		switch {
		case !ok && wasMissing:
			continue // still not published; nothing changes
		case !ok && !old:
			sum.Waiting++
			continue
		case !ok:
			r.settle(day, it, state.Missing, "not in GDELT's file list")
			sum.Missing++
		default:
			st, err := r.add(ctx, f)
			if err != nil && ctx.Err() != nil {
				return sum, days, ctx.Err()
			}
			var hard *runError
			switch {
			case errors.As(err, &hard):
				r.fail(day, it, prev, err)
				_ = state.Save(ctx, r.Store, day)
				return sum, days, err
			case err == nil && wasMissing:
				st.Recovered = true
				day.Files[it.key()] = st
				if day.CompactedAt != nil { // re-open the day; the next compaction merges the late batch in
					day.CompactedAt, day.Compacted = nil, nil
				}
				r.Log.Info("missing file arrived late; its day will be re-compacted", "file", f.Name)
				sum.Done++
				sum.Recovered++
			case err == nil:
				day.Files[it.key()] = st
				sum.Done++
			case wasMissing:
				continue // listed now but still not readable; stays missing until it is
			case old:
				r.settle(day, it, state.Missing, err.Error())
				sum.Missing++
			default:
				r.fail(day, it, prev, err)
				sum.Failed++
			}
		}
		if err := state.Save(ctx, r.Store, day); err != nil {
			return sum, days, err
		}
	}

	for _, date := range order {
		n, err := r.compact(ctx, days[date], now, lake.First())
		if err != nil {
			return sum, days, fmt.Errorf("compacting %s: %w", date, err)
		}
		sum.Compacted += n
	}
	if err := r.backfill(ctx, &lake, now, &sum); err != nil {
		return sum, days, err
	}
	return sum, days, nil
}

// backfillAttempts is how often a listed history file may fail before it's recorded as missing.
const backfillAttempts = 3

// backfill fetches history the lake should hold but doesn't, newest day first, until the target is reached or the
// run's budget is used up (design 8.5). It runs after the live window is up to date. Only system errors are returned.
func (r *Runner) backfill(ctx context.Context, lake *state.Lake, now time.Time, sum *Summary) error {
	if r.BackfillTarget == nil || r.BackfillBudget <= 0 {
		return nil
	}
	target := r.BackfillTarget(now).Truncate(source.BatchInterval)
	if target.IsZero() || !target.Before(lake.First()) {
		return nil
	}
	wall := r.wall
	if wall == nil {
		wall = time.Now
	}
	deadline := wall().Add(r.BackfillBudget)
	for ctx.Err() == nil && target.Before(lake.First()) {
		end := lake.First()
		from := end.Add(-time.Nanosecond).Truncate(24 * time.Hour) // the day holding the batch just before end
		if from.Before(target) {
			from = target
		}
		day, err := state.Load(ctx, r.Store, state.DateOf(from))
		if err != nil {
			return err
		}
		finished, err := r.backfillDay(ctx, day, from, end, deadline, wall, sum)
		if err != nil || !finished {
			if err == nil {
				r.Log.Info("backfill paused until the next run", "day", day.Date, "target", target)
			}
			return err
		}
		lake.Oldest = from
		if err := state.SaveLake(ctx, r.Store, *lake); err != nil {
			return err
		}
		sum.BackfillDays++
		day.CompactedAt, day.Compacted = nil, nil // the day gained batches; compact it again (a no-op merge if not)
		if _, err := r.compact(ctx, day, now, lake.First()); err != nil {
			return fmt.Errorf("compacting %s: %w", day.Date, err)
		}
		r.Log.Info("backfilled a day", "date", day.Date, "oldest", lake.First(), "target", target)
	}
	return ctx.Err()
}

// backfillDay settles every file in [from, end), one day's worth of history. It reports whether it finished before
// the deadline.
func (r *Runner) backfillDay(ctx context.Context, day *state.Day, from, end, deadline time.Time,
	wall func() time.Time, sum *Summary) (bool, error) {
	listed := map[string]source.File{}
	for _, s := range source.Streams {
		files, err := r.Source.Between(ctx, s, from, end)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			r.Log.Warn("couldn't read GDELT's file list for backfill; trying again next run", "day", day.Date, "error", err)
			sum.ListErrors++
			return false, nil
		}
		for _, f := range files {
			listed[f.Name] = f
		}
	}
	for b := from; b.Before(end); b = b.Add(source.BatchInterval) {
		for _, s := range source.Streams {
			for _, f := range source.Feeds {
				it := item{s, f, b}
				prev := day.Files[it.key()]
				if prev.Settled() {
					continue
				}
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				if !wall().Before(deadline) {
					return false, state.Save(ctx, r.Store, day)
				}
				lf, ok := listed[source.FileName(s, f, b)]
				if !ok {
					r.settle(day, it, state.Missing, "not in GDELT's file list")
					sum.BackfillMissing++
					continue
				}
				st, err := r.add(ctx, lf)
				var hard *runError
				switch {
				case err != nil && ctx.Err() != nil:
					return false, ctx.Err()
				case errors.As(err, &hard):
					r.fail(day, it, prev, err)
					_ = state.Save(ctx, r.Store, day)
					return false, err
				case err == nil:
					day.Files[it.key()] = st
					sum.BackfillDone++
				case prev != nil && prev.Attempts+1 >= backfillAttempts:
					r.settle(day, it, state.Missing, err.Error())
					day.Files[it.key()].Attempts = prev.Attempts + 1
					sum.BackfillMissing++
				default:
					r.fail(day, it, prev, err)
				}
				if err := state.Save(ctx, r.Store, day); err != nil {
					return false, err
				}
				if r.BackfillPause > 0 {
					select {
					case <-ctx.Done():
						return false, ctx.Err()
					case <-time.After(r.BackfillPause):
					}
				}
			}
		}
	}
	for b := from; b.Before(end); b = b.Add(source.BatchInterval) { // a file that failed this time keeps the day open
		for _, s := range source.Streams {
			for _, f := range source.Feeds {
				if !day.Files[state.Key(s, f, b)].Settled() {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// fail records a failed attempt at a file; it's retried on the next run.
func (r *Runner) fail(day *state.Day, it item, prev *state.File, err error) {
	now := r.Now().UTC()
	st := &state.File{Status: state.Failed, Attempts: 1, Error: err.Error(), Updated: now, FirstFailed: &now}
	if prev != nil && prev.Status == state.Failed {
		st.Attempts = prev.Attempts + 1
		if prev.FirstFailed != nil {
			st.FirstFailed = prev.FirstFailed
		}
	}
	day.Files[it.key()] = st
	r.Log.Warn("file failed; retrying next run", "file", source.FileName(it.stream, it.feed, it.batch),
		"attempts", st.Attempts, "error", err)
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

// runError marks an error that stops the run: the problem isn't the file but the system (DuckDB, storage, disk).
type runError struct{ err error }

func (e *runError) Error() string { return e.err.Error() }
func (e *runError) Unwrap() error { return e.err }

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
		return nil, &runError{err}
	}
	defer os.RemoveAll(tmp)
	// Download and unzip problems are about the file (soft); anything after that is about this system (hard).
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
		return nil, &runError{err}
	}
	_ = os.Remove(csv)
	size, err := r.put(ctx, out, LakePath(f.Stream, f.Feed, f.Batch))
	if err != nil {
		return nil, &runError{err}
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
		if ok, err := r.Store.Exists(ctx, DayPath(f, day.Date)); err != nil {
			return 0, err
		} else if ok { // re-compacting a re-opened day: merge the late batches into the existing day file
			inputs = append(inputs, DayPath(f, day.Date))
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

// ExitCode maps a run's outcome to the process exit code (design 8.1): 0 when the run did its job, whatever happened
// to individual files; 1 when it failed and needs attention. A run stopped on request exits 0.
func ExitCode(err error) int {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	return 1
}
