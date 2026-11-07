package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/j-c/gdelt-lake/internal/source"
	"github.com/j-c/gdelt-lake/internal/state"
	"github.com/j-c/gdelt-lake/internal/storage"
)

// HealthPath is where each run reports the lake's health (design 8.4).
const HealthPath = "_state/health.json"

// Thresholds for a healthy lake (design 8.4).
const (
	MaxBehind  = time.Hour // newest batch in the lake vs now, per stream
	MaxFailing = time.Hour // how long a file may keep failing
)

// Health is _state/health.json. It covers the lookback window; older history is in the per-day state files.
type Health struct {
	Updated  time.Time          `json:"updated"`
	Started  time.Time          `json:"lake_started"` // the lake's first batch
	Lookback string             `json:"lookback"`
	LastRun  RunReport          `json:"last_run"`
	Newest   map[string]*Newest `json:"newest"` // per stream ("en", "tr")
	Retrying []RetryingFile     `json:"retrying"`
	Gaps     []Gap              `json:"gaps"`
}

// RunReport is the outcome of the run that wrote the health file.
type RunReport struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Outcome  string    `json:"outcome"` // "ok" or "error"
	Error    string    `json:"error,omitempty"`
	Summary  Summary   `json:"summary"`
}

// Newest is the newest batch of a stream that's in the lake.
type Newest struct {
	Batch  time.Time `json:"batch"`
	Behind string    `json:"behind"` // at the time of the run, e.g. "17m0s"
}

// RetryingFile is a file that has failed and will be tried again.
type RetryingFile struct {
	File     string    `json:"file"`
	Since    time.Time `json:"since"`
	Attempts int       `json:"attempts"`
	Error    string    `json:"error"`
}

// Gap is a file recorded as missing ("open": still re-checked) or one that arrived late ("recovered").
type Gap struct {
	File   string `json:"file"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Summary's JSON names, for the health file.
func (s Summary) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int{"done": s.Done, "failed": s.Failed, "missing": s.Missing,
		"waiting": s.Waiting, "recovered": s.Recovered, "compacted": s.Compacted, "list_errors": s.ListErrors})
}

func (s *Summary) UnmarshalJSON(b []byte) error {
	var m map[string]int
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*s = Summary{Done: m["done"], Failed: m["failed"], Missing: m["missing"], Waiting: m["waiting"],
		Recovered: m["recovered"], Compacted: m["compacted"], ListErrors: m["list_errors"]}
	return nil
}

func (r *Runner) writeHealth(ctx context.Context, started time.Time, sum Summary, days map[string]*state.Day, runErr error) error {
	now := r.Now().UTC()
	h := Health{Updated: now, Lookback: r.Lookback.String(), Newest: map[string]*Newest{},
		LastRun: RunReport{Started: started, Finished: now, Outcome: "ok", Summary: sum}}
	if runErr != nil {
		h.LastRun.Outcome, h.LastRun.Error = "error", runErr.Error()
	}
	if lake, ok, err := state.LoadLake(ctx, r.Store); err == nil && ok {
		h.Started = lake.Started
	}
	if days == nil { // the run failed before reading state; keep what the last health file knew
		if prev, err := ReadHealth(ctx, r.Store); err == nil {
			h.Newest, h.Retrying, h.Gaps = prev.Newest, prev.Retrying, prev.Gaps
		}
	}
	for _, day := range days {
		for key, f := range day.Files {
			batch, stream, feed, ok := parseKey(key)
			if !ok {
				continue
			}
			name := source.FileName(stream, feed, batch)
			switch f.Status {
			case state.Done:
				if n := h.Newest[string(stream)]; n == nil || batch.After(n.Batch) {
					h.Newest[string(stream)] = &Newest{Batch: batch}
				}
				if f.Recovered {
					h.Gaps = append(h.Gaps, Gap{File: name, Status: "recovered"})
				}
			case state.Missing:
				h.Gaps = append(h.Gaps, Gap{File: name, Status: "open", Reason: f.Error})
			case state.Failed:
				since := f.Updated
				if f.FirstFailed != nil {
					since = *f.FirstFailed
				}
				h.Retrying = append(h.Retrying, RetryingFile{File: name, Since: since, Attempts: f.Attempts, Error: f.Error})
			}
		}
	}
	for _, n := range h.Newest {
		n.Behind = now.Sub(n.Batch).Round(time.Minute).String()
	}
	sort.Slice(h.Gaps, func(i, j int) bool { return h.Gaps[i].File < h.Gaps[j].File })
	sort.Slice(h.Retrying, func(i, j int) bool { return h.Retrying[i].File < h.Retrying[j].File })
	b, err := json.MarshalIndent(h, "", " ")
	if err != nil {
		return err
	}
	return r.Store.Write(ctx, HealthPath, bytes.NewReader(append(b, '\n')))
}

// parseKey reads a state key ("20261031T001500-en-gkg").
func parseKey(key string) (time.Time, source.Stream, source.Feed, bool) {
	parts := strings.Split(key, "-")
	if len(parts) != 3 {
		return time.Time{}, "", "", false
	}
	t, err := time.Parse("20060102T150405", parts[0])
	if err != nil {
		return time.Time{}, "", "", false
	}
	return t, source.Stream(parts[1]), source.Feed(parts[2]), true
}

// ReadHealth reads the lake's health file.
func ReadHealth(ctx context.Context, s storage.Storage) (Health, error) {
	rc, err := s.Open(ctx, HealthPath)
	if err != nil {
		return Health{}, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return Health{}, err
	}
	var h Health
	if err := json.Unmarshal(b, &h); err != nil {
		return Health{}, fmt.Errorf("%s: %w", HealthPath, err)
	}
	return h, nil
}

// Status codes of "gdelt-lake status" (design 8.4).
const (
	StatusHealthy = 0
	StatusSystem  = 1 // the last run failed, or the health file can't be read
	StatusData    = 3 // gaps, a lake behind, or files failing for long
)

// Evaluate judges a health report at time now: a status code and the reasons for it.
func Evaluate(h Health, now time.Time) (int, []string) {
	var system, data []string
	if h.LastRun.Outcome != "ok" {
		system = append(system, fmt.Sprintf("the last run (%s) failed: %s", h.LastRun.Finished.Format(time.RFC3339), h.LastRun.Error))
	}
	for _, s := range source.Streams {
		n := h.Newest[string(s)]
		switch {
		case n == nil && now.Sub(h.Started) <= MaxBehind:
			// a new lake: the translingual stream usually trails the English one by 15-30 minutes
		case n == nil:
			data = append(data, fmt.Sprintf("no %s batches in the lake's window", s))
		case now.Sub(n.Batch) > MaxBehind:
			data = append(data, fmt.Sprintf("%s is %s behind (newest batch %s)", s, now.Sub(n.Batch).Round(time.Minute),
				n.Batch.Format(time.RFC3339)))
		}
	}
	for _, f := range h.Retrying {
		if now.Sub(f.Since) > MaxFailing {
			data = append(data, fmt.Sprintf("%s failing for %s (%d attempts): %s", f.File, now.Sub(f.Since).Round(time.Minute),
				f.Attempts, f.Error))
		}
	}
	open := 0
	for _, g := range h.Gaps {
		if g.Status == "open" {
			open++
		}
	}
	if open > 0 {
		data = append(data, fmt.Sprintf("%d missing files in the window (gaps GDELT left; re-checked every run)", open))
	}
	switch {
	case len(system) > 0:
		return StatusSystem, append(system, data...)
	case len(data) > 0:
		return StatusData, data
	}
	return StatusHealthy, nil
}
