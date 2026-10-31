package state

import (
	"testing"
	"time"

	"github.com/j-c/gdelt-lake/internal/source"
	"github.com/j-c/gdelt-lake/internal/storage"
)

func TestKeyAndDate(t *testing.T) {
	b := time.Date(2026, 10, 31, 0, 15, 0, 0, time.UTC)
	if k := Key(source.Translingual, source.GKG, b); k != "20261031T001500-tr-gkg" {
		t.Fatalf("key %q", k)
	}
	if d := DateOf(b.In(time.FixedZone("AEST", 10*3600))); d != "2026-10-31" {
		t.Fatalf("date %q: must be the UTC date", d)
	}
}

func TestLoadSave(t *testing.T) {
	s := storage.NewMemory()
	d, err := Load(t.Context(), s, "2026-10-31")
	if err != nil || len(d.Files) != 0 || d.SchemaVersion != SchemaVersion {
		t.Fatalf("new day: %+v, %v", d, err)
	}
	d.Files["20261031T001500-en-gkg"] = &File{Status: Done, Rows: 42, Updated: time.Unix(0, 0).UTC()}
	now := time.Unix(100, 0).UTC()
	d.Compacted = map[source.Feed]*Compacted{source.GKG: {Rows: 42, Bytes: 1000}}
	d.CompactedAt = &now
	if err := Save(t.Context(), s, d); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Exists(t.Context(), "_state/2026-10-31.json"); !ok {
		t.Fatal("not saved where expected")
	}
	got, err := Load(t.Context(), s, "2026-10-31")
	if err != nil {
		t.Fatal(err)
	}
	f := got.Files["20261031T001500-en-gkg"]
	if !f.Settled() || f.Rows != 42 || got.Compacted[source.GKG].Rows != 42 || !got.CompactedAt.Equal(now) {
		t.Fatalf("round trip: %+v %+v", got, f)
	}
	if (&File{Status: Failed}).Settled() || (*File)(nil).Settled() {
		t.Fatal("failed or absent files aren't settled")
	}
}
