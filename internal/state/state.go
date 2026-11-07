// Package state keeps the lake's own record of progress: one JSON file per UTC day, at _state/YYYY-MM-DD.json, saying
// what happened to each of the day's files and whether the day has been compacted.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/j-c/gdelt-lake/internal/source"
	"github.com/j-c/gdelt-lake/internal/storage"
)

// SchemaVersion is the version of the lake's Parquet schema written by this code (design 6.1).
const SchemaVersion = 1

// Status is what happened to one GDELT file.
type Status string

const (
	Done    Status = "done"    // in the lake
	Failed  Status = "failed"  // tried and failed; retried on the next run
	Missing Status = "missing" // given up: GDELT never published it, or it couldn't be read in time
)

// File is the record for one GDELT file (one stream, one feed, one batch).
type File struct {
	Status   Status    `json:"status"`
	Attempts int       `json:"attempts,omitempty"`
	Error    string    `json:"error,omitempty"`
	Updated  time.Time `json:"updated"`
	// FirstFailed is when a failing file first failed, for "failing for how long".
	FirstFailed *time.Time `json:"first_failed,omitempty"`
	// Recovered marks a file that was recorded as missing and arrived later.
	Recovered bool `json:"recovered,omitempty"`
	// Set when done.
	Records           int64 `json:"records,omitempty"`
	Rows              int64 `json:"rows,omitempty"`
	Rejected          int64 `json:"rejected,omitempty"`
	ContinuationLines int64 `json:"continuation_lines,omitempty"`
	InvalidUTF8Lines  int64 `json:"invalid_utf8_lines,omitempty"`
	Bytes             int64 `json:"bytes,omitempty"`
}

// Compacted records one feed's day file.
type Compacted struct {
	Rows  int64 `json:"rows"`
	Bytes int64 `json:"bytes"`
}

// Day is the record for one UTC day.
type Day struct {
	SchemaVersion int                        `json:"schema_version"`
	Date          string                     `json:"date"` // YYYY-MM-DD
	Files         map[string]*File           `json:"files"`
	Compacted     map[source.Feed]*Compacted `json:"compacted,omitempty"`
	CompactedAt   *time.Time                 `json:"compacted_at,omitempty"`
}

// Key names a file within its day: "20261031T001500-en-gkg".
func Key(s source.Stream, f source.Feed, batch time.Time) string {
	return batch.UTC().Format("20060102T150405") + "-" + string(s) + "-" + string(f)
}

func path(date string) string { return "_state/" + date + ".json" }

// DateOf is the UTC date a batch belongs to.
func DateOf(batch time.Time) string { return batch.UTC().Format("2006-01-02") }

// Load reads a day's record, or returns an empty one if there is none yet.
func Load(ctx context.Context, s storage.Storage, date string) (*Day, error) {
	r, err := s.Open(ctx, path(date))
	if errors.Is(err, storage.ErrNotFound) {
		return &Day{SchemaVersion: SchemaVersion, Date: date, Files: map[string]*File{}}, nil
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var d Day
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", path(date), err)
	}
	if d.Files == nil {
		d.Files = map[string]*File{}
	}
	return &d, nil
}

// Save writes a day's record.
func Save(ctx context.Context, s storage.Storage, d *Day) error {
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	return s.Write(ctx, path(d.Date), bytes.NewReader(append(b, '\n')))
}

// Settled reports whether a file needs no more work: done or given up.
func (f *File) Settled() bool { return f != nil && (f.Status == Done || f.Status == Missing) }

// Lake records facts about the lake as a whole, at _state/lake.json.
type Lake struct {
	// Started is the first batch the lake collected live. Earlier batches come only from backfill.
	Started time.Time `json:"started"`
	// Oldest is how far back the lake is complete: every file from here on is in the lake or recorded as missing.
	// Backfill moves it back a day at a time; zero means Started.
	Oldest time.Time `json:"oldest,omitzero"`
}

// First is the oldest batch the lake holds without holes left to fill.
func (l Lake) First() time.Time {
	if !l.Oldest.IsZero() && l.Oldest.Before(l.Started) {
		return l.Oldest
	}
	return l.Started
}

const lakePath = "_state/lake.json"

// LoadLake reads the lake's record. ok is false if the lake is new.
func LoadLake(ctx context.Context, s storage.Storage) (lake Lake, ok bool, err error) {
	r, err := s.Open(ctx, lakePath)
	if errors.Is(err, storage.ErrNotFound) {
		return Lake{}, false, nil
	}
	if err != nil {
		return Lake{}, false, err
	}
	defer r.Close()
	if err := json.NewDecoder(r).Decode(&lake); err != nil {
		return Lake{}, false, fmt.Errorf("%s: %w", lakePath, err)
	}
	return lake, true, nil
}

// SaveLake writes the lake's record.
func SaveLake(ctx context.Context, s storage.Storage, lake Lake) error {
	b, err := json.MarshalIndent(lake, "", " ")
	if err != nil {
		return err
	}
	return s.Write(ctx, lakePath, bytes.NewReader(append(b, '\n')))
}
