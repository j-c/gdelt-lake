package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j-c/gdelt-lake/internal/convert"
	"github.com/j-c/gdelt-lake/internal/source"
	"github.com/j-c/gdelt-lake/internal/source/fakegdelt"
	"github.com/j-c/gdelt-lake/internal/state"
	"github.com/j-c/gdelt-lake/internal/storage"
)

// One valid record per feed: small, so a full day of batches converts quickly.
var rows = map[string]string{
	"events":   "1271880431\t20261030\t202610\t2026\t2026.8247" + strings.Repeat("\t", 55) + "20261030141500\thttps://example.org/a\n",
	"mentions": "1271880431\t20261030141500\t20261030141500\t1\texample.org\thttps://example.org/a\t1\t-1\t-1\t-1\t1\t50\t1000\t-1.5\t\t\n",
	"gkg":      "20261030141500-1\t20261030141500\t1\texample.org\thttps://example.org/a" + strings.Repeat("\t", 22) + "<PAGE_TITLE>A</PAGE_TITLE>\n",
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type env struct {
	srv   *fakegdelt.Server
	store storage.Storage
	clock *clock
	r     *Runner
}

func newEnv(t *testing.T, now time.Time) *env {
	t.Helper()
	srv := fakegdelt.New(t)
	eng, err := convert.Open("256MB", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	c := &clock{now}
	e := &env{srv: srv, store: storage.NewMemory(), clock: c}
	e.r = &Runner{Store: e.store, Source: source.NewClient(srv.BaseURL(), "test").WithBackoff(time.Millisecond), Engine: eng, TempDir: t.TempDir(),
		Lookback: 7 * 24 * time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: c.now}
	return e
}

func (e *env) publish(b time.Time, faults map[string]fakegdelt.Fault, streams ...string) {
	if len(streams) == 0 {
		streams = []string{"en", "tr"}
	}
	for _, s := range streams {
		e.srv.AddBatch(s, b, rows, faults)
	}
}

func (e *env) run(t *testing.T) Summary {
	t.Helper()
	sum, err := e.r.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func (e *env) file(t *testing.T, s source.Stream, f source.Feed, b time.Time) *state.File {
	t.Helper()
	d, err := state.Load(t.Context(), e.store, state.DateOf(b))
	if err != nil {
		t.Fatal(err)
	}
	return d.Files[state.Key(s, f, b)]
}

var day = time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)

func TestFirstRunIsForwardOnly(t *testing.T) {
	e := newEnv(t, day.Add(10*time.Hour+7*time.Minute))
	e.publish(day.Add(9*time.Hour), nil) // before the lake starts: never fetched
	e.publish(day.Add(10*time.Hour), nil)
	sum := e.run(t)
	// 10:00 is done (both streams, 3 feeds); 10:15 isn't published yet.
	if sum.Done != 6 || sum.Failed != 0 || sum.Missing != 0 || sum.Waiting != 6 {
		t.Fatalf("summary %+v", sum)
	}
	lake, ok, err := state.LoadLake(t.Context(), e.store)
	if err != nil || !ok || !lake.Started.Equal(day.Add(10*time.Hour)) {
		t.Fatalf("lake %+v %v %v", lake, ok, err)
	}
	if ok, _ := e.store.Exists(t.Context(), LakePath(source.English, source.GKG, day.Add(10*time.Hour))); !ok {
		t.Fatal("10:00 GKG isn't in the lake")
	}
	if ok, _ := e.store.Exists(t.Context(), LakePath(source.English, source.GKG, day.Add(9*time.Hour))); ok {
		t.Fatal("09:00 was fetched, before the lake started")
	}
	f := e.file(t, source.Translingual, source.GKG, day.Add(10*time.Hour))
	if f.Status != state.Done || f.Rows != 1 || f.Records != 1 {
		t.Fatalf("state %+v", f)
	}

	// A rerun with nothing new does nothing.
	if sum := e.run(t); sum.Done != 0 || sum.Failed != 0 || sum.Waiting != 6 {
		t.Fatalf("rerun summary %+v", sum)
	}
	if n := e.srv.Requests(fakegdelt.Name("en", "gkg", day.Add(10*time.Hour))); n != 1 {
		t.Fatalf("10:00 GKG downloaded %d times, want once", n)
	}
	if code := ExitCode(nil); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestFailedFileIsRetriedNextRun(t *testing.T) {
	start := day.Add(10 * time.Hour)
	e := newEnv(t, start.Add(time.Minute))
	e.publish(start, map[string]fakegdelt.Fault{"mentions": fakegdelt.Corrupt}, "en")
	e.publish(start, nil, "tr")
	sum := e.run(t) // a failed file is soft: the run still did its job
	if sum.Done != 5 || sum.Failed != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if f := e.file(t, source.English, source.Mentions, start); f.Status != state.Failed || f.Attempts != 1 || f.Error == "" {
		t.Fatalf("state %+v", f)
	}
	e.publish(start, nil, "en") // GDELT fixes the file
	e.clock.t = start.Add(16 * time.Minute)
	sum = e.run(t)
	if sum.Done != 1 || sum.Failed != 0 {
		t.Fatalf("second run %+v", sum)
	}
	if f := e.file(t, source.English, source.Mentions, start); f.Status != state.Done {
		t.Fatalf("state after retry %+v", f)
	}
}

func TestGapsAreRecordedAsMissing(t *testing.T) {
	start := day.Add(10 * time.Hour)
	e := newEnv(t, start.Add(time.Minute))
	e.publish(start, map[string]fakegdelt.Fault{"events": fakegdelt.Missing, "gkg": fakegdelt.Unlisted}, "en")
	e.publish(start, nil, "tr")
	sum := e.run(t)
	// Young: the 404 counts as failed, the unlisted file as waiting.
	if sum.Done != 4 || sum.Failed != 1 || sum.Missing != 0 {
		t.Fatalf("young summary %+v", sum)
	}
	// Seven hours later, both are given up on and recorded.
	e.clock.t = start.Add(7 * time.Hour)
	e.publish(start.Add(15*time.Minute), nil) // a later batch, so the run reads the master list
	sum = e.run(t)
	if sum.Missing < 2 || sum.Failed != 0 {
		t.Fatalf("later summary %+v", sum) // also counts later slots that never appeared and are now 6 h old
	}
	for f, why := range map[source.Feed]string{source.Events: "not published", source.GKG: "not in GDELT's file list"} {
		st := e.file(t, source.English, f, start)
		if st.Status != state.Missing || !strings.Contains(st.Error, why) {
			t.Errorf("%s: state %+v, want missing because %q", f, st, why)
		}
	}
}

func TestDayIsCompacted(t *testing.T) {
	started := day.Add(6 * time.Hour) // the lake starts mid-day: the first day is partial
	e := newEnv(t, started.Add(time.Minute))
	e.publish(started, nil)
	if sum := e.run(t); sum.Done != 6 {
		t.Fatalf("first run %+v", sum)
	}
	// The rest of the day is published while the ingester is down; it catches up the next morning.
	for b := started.Add(source.BatchInterval); b.Before(day.Add(24 * time.Hour)); b = b.Add(source.BatchInterval) {
		e.publish(b, nil)
	}
	e.clock.t = day.Add(24*time.Hour + CompactGrace + time.Minute)
	sum := e.run(t)
	batches := int((18 * time.Hour) / source.BatchInterval) // 06:00 to 23:45
	if sum.Done != (batches-1)*6 || sum.Failed != 0 || sum.Compacted != 1 {
		t.Fatalf("catch-up summary %+v", sum)
	}
	date := state.DateOf(day)
	d, err := state.Load(t.Context(), e.store, date)
	if err != nil || d.CompactedAt == nil {
		t.Fatalf("day %+v %v", d, err)
	}
	for _, f := range source.Feeds {
		if c := d.Compacted[f]; c == nil || c.Rows != int64(batches*2) {
			t.Errorf("%s: compacted %+v, want %d rows", f, c, batches*2)
		}
		files, _ := e.store.List(t.Context(), string(f)+"/date="+date+"/")
		if len(files) != 1 || files[0] != DayPath(f, date) {
			t.Errorf("%s: files after compaction %v, want only the day file", f, files)
		}
	}

	// A run that stopped between recording the compaction and deleting the batch files is finished next time.
	stray := LakePath(source.English, source.GKG, started)
	if err := e.store.Write(t.Context(), stray, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if sum := e.run(t); sum.Compacted != 0 || sum.Done != 0 {
		t.Fatalf("rerun %+v", sum)
	}
	if ok, _ := e.store.Exists(t.Context(), stray); ok {
		t.Fatal("leftover batch file not removed")
	}
}

func TestCompactionWaitsForTheWholeDay(t *testing.T) {
	started := day.Add(23 * time.Hour)
	e := newEnv(t, started.Add(time.Minute))
	e.publish(started, nil)
	e.run(t)
	e.publish(started.Add(15*time.Minute), nil, "en") // the translingual 23:15 batch never appears...
	e.clock.t = day.Add(24*time.Hour + CompactGrace + time.Minute)
	if sum := e.run(t); sum.Compacted != 0 {
		t.Fatalf("compacted before every file was accounted for: %+v", sum)
	}
	// ...until it's old enough to be recorded as missing (6 hours), then the day compacts.
	e.publish(day.Add(31*time.Hour), nil) // later batches exist, so the list is read back far enough
	e.clock.t = day.Add(30 * time.Hour)   // 23:15, 23:30 and 23:45 are now more than 6 hours old
	if sum := e.run(t); sum.Compacted != 1 {
		t.Fatalf("not compacted once the gap was recorded: %+v", sum)
	}
}

func TestStopsWhenCancelled(t *testing.T) {
	e := newEnv(t, day.Add(10*time.Hour+time.Minute))
	e.publish(day.Add(10*time.Hour), nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sum, err := e.r.Run(ctx)
	if err == nil || ExitCode(err) != 0 {
		t.Fatalf("summary %+v, err %v, exit %d: a stop is a clean exit", sum, err, ExitCode(err))
	}
}

func TestLocalLakeEndToEnd(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lake")
	local, err := storage.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, day.Add(10*time.Hour+time.Minute))
	e.r.Store = local
	e.store = local
	e.publish(day.Add(10*time.Hour), nil)
	if sum := e.run(t); sum.Done != 6 {
		t.Fatalf("summary %+v", sum)
	}
	if _, err := os.Stat(filepath.Join(root, "gkg", "date=2026-10-30", "20261030T100000-tr.parquet")); err != nil {
		t.Fatal(err)
	}
}

func (e *env) health(t *testing.T) Health {
	t.Helper()
	h, err := ReadHealth(t.Context(), e.store)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// GDELT sometimes lists a file before it can be downloaded (seen for up to about 45 minutes in the first soak).
func TestListedButNotDownloadableYetIsSoft(t *testing.T) {
	start := day.Add(10 * time.Hour)
	e := newEnv(t, start.Add(2*time.Minute))
	e.publish(start, map[string]fakegdelt.Fault{"gkg": fakegdelt.Missing}, "en")
	e.publish(start, nil, "tr")
	sum, err := e.r.Run(t.Context())
	if err != nil || ExitCode(err) != 0 || sum.Failed != 1 || sum.Done != 5 {
		t.Fatalf("summary %+v, err %v", sum, err)
	}
	h := e.health(t)
	if h.LastRun.Outcome != "ok" || len(h.Retrying) != 1 || h.Retrying[0].File != fakegdelt.Name("en", "gkg", start) {
		t.Fatalf("health %+v", h)
	}
	if code, why := Evaluate(h, e.clock.t); code != StatusHealthy {
		t.Fatalf("status %d %v: a file failing for minutes isn't a problem yet", code, why)
	}
	// Still failing 90 minutes later: a data problem, not a system one.
	e.clock.t = start.Add(90 * time.Minute)
	e.run(t)
	if code, why := Evaluate(e.health(t), e.clock.t); code != StatusData || !strings.Contains(strings.Join(why, ";"), "failing for") {
		t.Fatalf("status %d %v", code, why)
	}
	if f := e.file(t, source.English, source.GKG, start); f.FirstFailed == nil || !f.FirstFailed.Equal(start.Add(2*time.Minute)) || f.Attempts != 2 {
		t.Fatalf("state %+v: want the first failure time kept", f)
	}
	// GDELT catches up.
	e.publish(start, nil, "en")
	e.clock.t = start.Add(105 * time.Minute)
	if sum := e.run(t); sum.Done != 1 || sum.Failed != 0 {
		t.Fatalf("after GDELT caught up: %+v", sum)
	}
	if h := e.health(t); len(h.Retrying) != 0 {
		t.Fatalf("still retrying %+v", h.Retrying)
	}
}

func TestLateFileReopensAndRecompactsItsDay(t *testing.T) {
	started := day.Add(23 * time.Hour)
	e := newEnv(t, started.Add(time.Minute))
	e.publish(started, map[string]fakegdelt.Fault{"events": fakegdelt.Missing}, "en")
	e.publish(started, nil, "tr")
	e.run(t) // the lake starts at 23:00; the English events file is listed but 404
	for b := started.Add(15 * time.Minute); b.Before(day.Add(24 * time.Hour)); b = b.Add(15 * time.Minute) {
		e.publish(b, nil)
	}
	e.publish(day.Add(30*time.Hour), nil) // a later batch, so the list reaches back
	// The next morning: the 23:00 English events file is still 404, so it's recorded as missing and the day compacts.
	e.clock.t = day.Add(30 * time.Hour)
	sum := e.run(t)
	if sum.Compacted != 1 || sum.Missing != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if code, why := Evaluate(e.health(t), e.clock.t); code != StatusData || !strings.Contains(strings.Join(why, ";"), "1 missing") {
		t.Fatalf("status %d %v: an open gap is a data problem", code, why)
	}
	// GDELT publishes it late; the next run adds it, re-opens the day and re-compacts it.
	e.publish(started, nil, "en")
	e.clock.t = day.Add(31 * time.Hour)
	sum = e.run(t)
	if sum.Recovered != 1 || sum.Compacted != 1 {
		t.Fatalf("summary after the late file %+v", sum)
	}
	d, err := state.Load(t.Context(), e.store, state.DateOf(day))
	if err != nil || d.CompactedAt == nil || d.Compacted[source.Events].Rows != 8 { // 4 batches x 2 streams x 1 row
		t.Fatalf("day %+v %v", d.Compacted[source.Events], err)
	}
	files, _ := e.store.List(t.Context(), "events/date="+state.DateOf(day)+"/")
	if len(files) != 1 {
		t.Fatalf("files %v: want only the re-compacted day file", files)
	}
	// (The next day's first batches were never published in this test, so they show as open gaps too.)
	var recovered []string
	for _, g := range e.health(t).Gaps {
		if g.Status == "recovered" {
			recovered = append(recovered, g.File)
		} else if strings.HasPrefix(g.File, "20261030") {
			t.Errorf("day %s still has an open gap: %+v", state.DateOf(day), g)
		}
	}
	if len(recovered) != 1 || recovered[0] != fakegdelt.Name("en", "events", started) {
		t.Fatalf("recovered gaps %v", recovered)
	}
}

// failingWrites fails writes of Parquet files: a stand-in for a broken disk or storage account.
type failingWrites struct{ storage.Storage }

func (f failingWrites) Write(ctx context.Context, p string, r io.Reader) error {
	if strings.HasSuffix(p, ".parquet") {
		return errors.New("disk full")
	}
	return f.Storage.Write(ctx, p, r)
}

func TestSystemErrorStopsTheRun(t *testing.T) {
	e := newEnv(t, day.Add(10*time.Hour+time.Minute))
	e.r.Store = failingWrites{e.store}
	e.publish(day.Add(10*time.Hour), nil)
	sum, err := e.r.Run(t.Context())
	if err == nil || ExitCode(err) != 1 || sum.Done != 0 {
		t.Fatalf("summary %+v, err %v: a storage failure stops the run with exit 1", sum, err)
	}
	h := e.health(t)
	if h.LastRun.Outcome != "error" || !strings.Contains(h.LastRun.Error, "disk full") {
		t.Fatalf("health %+v", h.LastRun)
	}
	if code, _ := Evaluate(h, e.clock.t); code != StatusSystem {
		t.Fatalf("status %d, want %d (system)", code, StatusSystem)
	}
}

func TestEvaluate(t *testing.T) {
	now := day.Add(12 * time.Hour)
	fresh := map[string]*Newest{"en": {Batch: now.Add(-20 * time.Minute)}, "tr": {Batch: now.Add(-40 * time.Minute)}}
	ok := Health{LastRun: RunReport{Outcome: "ok"}, Newest: fresh}
	if code, why := Evaluate(ok, now); code != StatusHealthy {
		t.Fatalf("%d %v", code, why)
	}
	behind := Health{LastRun: RunReport{Outcome: "ok"}, Newest: map[string]*Newest{"en": fresh["en"],
		"tr": {Batch: now.Add(-3 * time.Hour)}}}
	if code, why := Evaluate(behind, now); code != StatusData || !strings.Contains(why[0], "tr is 3h0m0s behind") {
		t.Fatalf("%d %v", code, why)
	}
	newLake := Health{LastRun: RunReport{Outcome: "ok"}, Started: now.Add(-10 * time.Minute),
		Newest: map[string]*Newest{"en": fresh["en"]}}
	if code, why := Evaluate(newLake, now); code != StatusHealthy {
		t.Fatalf("a 10-minute-old lake without translingual batches yet: %d %v", code, why)
	}
	newLake.Started = now.Add(-2 * time.Hour)
	if code, _ := Evaluate(newLake, now); code != StatusData {
		t.Fatalf("no translingual batches after 2 hours: %d", code)
	}
	failed := Health{LastRun: RunReport{Outcome: "error", Error: "duckdb: out of memory"}, Newest: fresh}
	if code, _ := Evaluate(failed, now); code != StatusSystem {
		t.Fatalf("%d", code)
	}
}

// backfillEnv is a lake started at 02:00 on day+1, with history published from day 00:00 and a backfill target of
// day 20:00: 16 batches of history on day and 8 on day+1.
var (
	bfStarted = day.Add(26 * time.Hour)
	bfTarget  = day.Add(20 * time.Hour)
	bfGap     = day.Add(21 * time.Hour) // GDELT never published this translingual batch
)

func backfillEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, bfStarted.Add(time.Minute))
	for b := day; !b.After(bfStarted); b = b.Add(15 * time.Minute) {
		if b.Equal(bfGap) {
			e.publish(b, nil, "en")
			continue
		}
		e.publish(b, nil)
	}
	e.r.BackfillTarget = func(time.Time) time.Time { return bfTarget }
	e.r.BackfillBudget = time.Hour
	return e
}

func (e *env) lake(t *testing.T) state.Lake {
	t.Helper()
	lake, _, err := state.LoadLake(t.Context(), e.store)
	if err != nil {
		t.Fatal(err)
	}
	return lake
}

func TestBackfillAfterLive(t *testing.T) {
	e := backfillEnv(t)
	sum := e.run(t)
	// Live first (the current batch), then history.
	if sum.Done != 6 || sum.BackfillDays != 2 {
		t.Fatalf("summary %+v", sum)
	}
	if want := (16 + 8) * 6; sum.BackfillDone+sum.BackfillMissing != want || sum.BackfillMissing != 3 {
		t.Fatalf("backfill %d done, %d missing; want %d in all, 3 missing", sum.BackfillDone, sum.BackfillMissing, want)
	}
	if lake := e.lake(t); !lake.Started.Equal(bfStarted) || !lake.Oldest.Equal(bfTarget) {
		t.Fatalf("lake %+v", lake)
	}
	// A finished history day is compacted straight away; day+1 waits for its end like any day.
	d, _ := state.Load(t.Context(), e.store, state.DateOf(day))
	if d.CompactedAt == nil || d.Compacted[source.GKG].Rows != 16*2-1 {
		t.Fatalf("day: %v %+v", d.CompactedAt, d.Compacted[source.GKG])
	}
	if d, _ := state.Load(t.Context(), e.store, state.DateOf(bfStarted)); d.CompactedAt != nil {
		t.Fatal("day+1 compacted before it ended")
	}
	if h := e.health(t); h.Backfill == nil || h.Backfill.DaysRemaining != 0 || !h.Backfill.Oldest.Equal(bfTarget) {
		t.Fatalf("health backfill %+v", h.Backfill)
	}
	// Nothing left: the next run doesn't touch history.
	if sum := e.run(t); sum.BackfillDone != 0 || sum.BackfillDays != 0 {
		t.Fatalf("second run %+v", sum)
	}
	// When day+1 ends, its compaction includes the backfilled early hours and the live batches.
	for b := bfStarted.Add(15 * time.Minute); b.Before(day.Add(48 * time.Hour)); b = b.Add(15 * time.Minute) {
		e.publish(b, nil)
	}
	e.clock.t = day.Add(50*time.Hour + time.Minute)
	e.run(t)
	d, _ = state.Load(t.Context(), e.store, state.DateOf(bfStarted))
	if d.CompactedAt == nil || d.Compacted[source.Events].Rows != 96*2 {
		t.Fatalf("day+1 %+v %v", d.Compacted[source.Events], d.CompactedAt)
	}
}

func TestBackfillBudgetNewestDayFirst(t *testing.T) {
	e := backfillEnv(t)
	tick := time.Unix(0, 0)
	e.r.wall = func() time.Time { tick = tick.Add(time.Second); return tick } // about a second per file
	e.r.BackfillBudget = time.Minute
	// Run 1: day+1's 48 files finish first (newest day), then the budget stops it partway through day.
	if sum := e.run(t); sum.BackfillDays != 1 || sum.BackfillDone+sum.BackfillMissing >= 96 {
		t.Fatalf("run 1 %+v", sum)
	}
	if lake := e.lake(t); !lake.Oldest.Equal(day.Add(24 * time.Hour)) {
		t.Fatalf("after run 1 the lake is complete from %v, want day+1 00:00", lake.Oldest)
	}
	runs := 1
	for !e.lake(t).Oldest.Equal(bfTarget) {
		if runs++; runs > 5 {
			t.Fatal("backfill never finished")
		}
		e.run(t)
	}
	if runs != 3 {
		t.Fatalf("finished in %d runs, want 3 for 144 files at about 59 a run", runs)
	}
	d, _ := state.Load(t.Context(), e.store, state.DateOf(day))
	if d.CompactedAt == nil || d.Compacted[source.Mentions].Rows != 16*2-1 {
		t.Fatalf("day %+v", d.Compacted)
	}
}

func TestBackfillPausedAndRaised(t *testing.T) {
	e := backfillEnv(t)
	e.r.BackfillBudget = 0
	if sum := e.run(t); sum.BackfillDone != 0 {
		t.Fatalf("paused backfill ran: %+v", sum)
	}
	if h := e.health(t); h.Backfill == nil || !h.Backfill.Paused || h.Backfill.DaysRemaining != 2 {
		t.Fatalf("health %+v", h.Backfill)
	}
	// A target of day+1 00:00 first, then raised to day 20:00.
	e.r.BackfillBudget = time.Hour
	e.r.BackfillTarget = func(time.Time) time.Time { return day.Add(24 * time.Hour) }
	if sum := e.run(t); sum.BackfillDays != 1 || sum.BackfillDone != 48 {
		t.Fatalf("%+v", sum)
	}
	e.r.BackfillTarget = func(time.Time) time.Time { return bfTarget }
	if sum := e.run(t); sum.BackfillDays != 1 || sum.BackfillDone != 16*6-3 {
		t.Fatalf("%+v", sum)
	}
	if lake := e.lake(t); !lake.Oldest.Equal(bfTarget) || !lake.Started.Equal(bfStarted) {
		t.Fatalf("%+v", lake)
	}
}

func TestBackfillRetriesAFailingFileThenGivesUp(t *testing.T) {
	e := backfillEnv(t)
	e.r.BackfillTarget = func(time.Time) time.Time { return day.Add(24 * time.Hour) }
	bad := day.Add(25 * time.Hour)
	e.publish(bad, map[string]fakegdelt.Fault{"gkg": fakegdelt.Corrupt}, "en")
	for i := 1; i <= backfillAttempts; i++ {
		sum := e.run(t)
		if done := e.lake(t).Oldest.Equal(day.Add(24 * time.Hour)); done != (i == backfillAttempts) {
			t.Fatalf("attempt %d: day finished %v (%+v)", i, done, sum)
		}
	}
	if f := e.file(t, source.English, source.GKG, bad); f.Status != state.Missing || f.Attempts != backfillAttempts {
		t.Fatalf("%+v", f)
	}
}

// A history day that ended less than CompactGrace ago is compacted by a later run, and its gaps are re-checked like
// live ones.
func TestBackfilledRecentDayCompactsLater(t *testing.T) {
	e := newEnv(t, day.Add(24*time.Hour+31*time.Minute))
	for b := day.Add(20 * time.Hour); !b.After(day.Add(24*time.Hour + 30*time.Minute)); b = b.Add(15 * time.Minute) {
		if b.Equal(bfGap) {
			e.publish(b, nil, "en")
			continue
		}
		e.publish(b, nil)
	}
	e.r.BackfillTarget = func(time.Time) time.Time { return bfTarget }
	e.r.BackfillBudget = time.Hour
	if sum := e.run(t); sum.BackfillDays != 2 {
		t.Fatalf("%+v", sum)
	}
	if d, _ := state.Load(t.Context(), e.store, state.DateOf(day)); d.CompactedAt != nil {
		t.Fatal("compacted within the grace period")
	}
	// The missing translingual batch turns up; the next run after the grace period adds it and compacts the day.
	e.publish(bfGap, nil, "tr")
	e.clock.t = day.Add(26*time.Hour + time.Minute)
	sum := e.run(t)
	d, _ := state.Load(t.Context(), e.store, state.DateOf(day))
	if sum.Recovered != 3 || d.CompactedAt == nil || d.Compacted[source.Events].Rows != 16*2 {
		t.Fatalf("summary %+v, day %v %+v", sum, d.CompactedAt, d.Compacted[source.Events])
	}
}
