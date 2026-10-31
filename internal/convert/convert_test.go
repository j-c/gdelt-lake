package convert

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/j-c/gdelt-lake/internal/source"
)

func TestClean(t *testing.T) {
	in := "" +
		"20261030141500-T1\t20261030141500\tfirst\textras with a link?body=\n" +
		"___________________\n" + // continuation: the field had a line break
		"more of the same field\n" +
		"\n" +
		"20261030141500-T2\t20261030141500\tbad \xff byte\textras\r\n" +
		"20261030141500-3\t20261030141500\tthird\textras"
	var out bytes.Buffer
	st, err := clean(strings.NewReader(in), &out, recordStart(source.GKG))
	if err != nil {
		t.Fatal(err)
	}
	want := "" +
		"20261030141500-T1\t20261030141500\tfirst\textras with a link?body= ___________________ more of the same field\n" +
		"20261030141500-T2\t20261030141500\tbad \uFFFD byte\textras\n" +
		"20261030141500-3\t20261030141500\tthird\textras\n"
	if out.String() != want {
		t.Fatalf("cleaned:\n%q\nwant:\n%q", out.String(), want)
	}
	if st != (CleanStats{Records: 3, ContinuationLines: 2, InvalidUTF8Lines: 1}) {
		t.Fatalf("stats %+v", st)
	}
}

func TestRecordStartPerFeed(t *testing.T) {
	for feed, line := range map[source.Feed]string{
		source.Events:   "1271880431\t20251030\t202510\t2025\t2025.8192\t",
		source.Mentions: "1271880431\t20261030141500\t20261030141500\t1\t",
		source.GKG:      "20261030141500-T0\t20261030141500\t1\t",
	} {
		if !recordStart(feed).MatchString(line) {
			t.Errorf("%s: %q not recognised as a record start", feed, line)
		}
		if recordStart(feed).MatchString("https://example.org/page\tx") {
			t.Errorf("%s: a URL line taken for a record start", feed)
		}
	}
}

func newEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := Open("512MB", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// fixture copies a testdata file to a temp dir (Batch writes its cleaned copy next to the input).
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBatchAllFixtures(t *testing.T) {
	e := newEngine(t)
	for _, stream := range []string{"en", "tr"} {
		for _, feed := range source.Feeds {
			name := stream + "." + string(feed) + ".csv"
			t.Run(name, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "out.parquet")
				r, err := e.Batch(t.Context(), feed, stream == "tr", fixture(t, name), out)
				if err != nil {
					t.Fatal(err)
				}
				if r.Rows == 0 || r.Rejected() != 0 {
					t.Fatalf("%+v: want every record read", r)
				}
				var translated int64
				if err := e.DB().QueryRow("SELECT count(*) FROM read_parquet(?) WHERE translated = ?", out, stream == "tr").Scan(&translated); err != nil {
					t.Fatal(err)
				}
				if translated != r.Rows {
					t.Fatalf("%d of %d rows have translated = %v", translated, r.Rows, stream == "tr")
				}
			})
		}
	}
}

func columns(t *testing.T, e *Engine, path string) []string {
	t.Helper()
	rows, err := e.DB().Query("SELECT column_name FROM (DESCRIBE SELECT * FROM read_parquet(" + literal(path) + "))")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestGKGParsedFields(t *testing.T) {
	e := newEngine(t)
	out := filepath.Join(t.TempDir(), "tr.gkg.parquet")
	r, err := e.Batch(t.Context(), source.GKG, true, fixture(t, "tr.gkg.csv"), out)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture holds 15 records: two whose Extras contain line breaks (6 lines in all) and one with invalid UTF-8.
	if r.Records != 15 || r.Rows != 15 || r.ContinuationLines != 6 || r.InvalidUTF8Lines < 1 {
		t.Fatalf("result %+v", r)
	}

	cols := columns(t, e, out)
	for _, c := range []string{"page_title", "themes", "locations", "persons", "organizations", "tone", "translation"} {
		if !slices.Contains(cols, c) {
			t.Errorf("no %s column in %v", c, cols)
		}
	}
	for _, c := range []string{"gcam", "v2_themes", "v2_persons", "v1_themes"} {
		if slices.Contains(cols, c) {
			t.Errorf("unexpected column %s", c)
		}
	}

	// The first record, checked by hand against the published line.
	var (
		date                       time.Time
		title, person, lang, eng   string
		personOffset, words, nLocs int
		lat                        float64
		country                    string
	)
	err = e.DB().QueryRow(`SELECT date, page_title, persons[1].name, persons[1].char_offset,
			translation.source_language, translation.engine, tone.word_count,
			len(locations), locations[1].lat, locations[1].country_code
		FROM read_parquet(?) WHERE gkg_record_id = '20261030141500-T0'`, out).
		Scan(&date, &title, &person, &personOffset, &lang, &eng, &words, &nLocs, &lat, &country)
	if err != nil {
		t.Fatal(err)
	}
	if !date.Equal(time.Date(2026, 10, 30, 14, 15, 0, 0, time.UTC)) || person != "Violet Christmas" || personOffset != 848 ||
		lang != "hrv" || eng != "GT-HRV 1.0" || words != 460 || nLocs == 0 || lat != 45.166667 || country != "HR" ||
		!strings.HasPrefix(title, "FOTO Uo") {
		t.Fatalf("first record: date %v title %q person %q@%d lang %q eng %q words %d locations %d (%v, %q)",
			date, title, person, personOffset, lang, eng, words, nLocs, lat, country)
	}

	// A record whose Extras had line breaks keeps its whole Extras field, joined with spaces.
	var joined int
	if err := e.DB().QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE extras LIKE '%body= %'`, out).Scan(&joined); err != nil {
		t.Fatal(err)
	}
	if joined == 0 {
		t.Error("no record has its joined Extras field")
	}
}

func TestEnglishGKGHasNoTranslation(t *testing.T) {
	e := newEngine(t)
	out := filepath.Join(t.TempDir(), "en.gkg.parquet")
	if _, err := e.Batch(t.Context(), source.GKG, false, fixture(t, "en.gkg.csv"), out); err != nil {
		t.Fatal(err)
	}
	var withTranslation, withTitle int
	if err := e.DB().QueryRow(`SELECT count(translation), count(page_title) FROM read_parquet(?)`, out).
		Scan(&withTranslation, &withTitle); err != nil {
		t.Fatal(err)
	}
	if withTranslation != 0 || withTitle == 0 {
		t.Fatalf("%d rows with translation info (want 0), %d with a title (want some)", withTranslation, withTitle)
	}
}

func TestEventsAndMentionsTypes(t *testing.T) {
	e := newEngine(t)
	ev := filepath.Join(t.TempDir(), "events.parquet")
	if _, err := e.Batch(t.Context(), source.Events, false, fixture(t, "en.events.csv"), ev); err != nil {
		t.Fatal(err)
	}
	var id int64
	var day, added time.Time
	var root bool
	if err := e.DB().QueryRow(`SELECT global_event_id, sql_date, date_added, is_root_event FROM read_parquet(?) LIMIT 1`, ev).
		Scan(&id, &day, &added, &root); err != nil {
		t.Fatal(err)
	}
	if id == 0 || day.IsZero() || !added.Equal(time.Date(2026, 10, 30, 14, 15, 0, 0, time.UTC)) {
		t.Fatalf("events: id %d sql_date %v date_added %v", id, day, added)
	}
	me := filepath.Join(t.TempDir(), "mentions.parquet")
	if _, err := e.Batch(t.Context(), source.Mentions, true, fixture(t, "tr.mentions.csv"), me); err != nil {
		t.Fatal(err)
	}
	var mentioned time.Time
	var confidence int
	if err := e.DB().QueryRow(`SELECT mention_time, confidence FROM read_parquet(?) LIMIT 1`, me).Scan(&mentioned, &confidence); err != nil {
		t.Fatal(err)
	}
	if mentioned.IsZero() || confidence <= 0 || confidence > 100 {
		t.Fatalf("mentions: mention_time %v confidence %d", mentioned, confidence)
	}
}

func TestBadRows(t *testing.T) {
	e := newEngine(t)
	// One good row, one with too few columns (rejected), one with a date that isn't a date (kept, NULL date).
	good := "20261030141500-T1\t20261030141500" + strings.Repeat("\tx", 25)
	short := "20261030141500-T2\t20261030141500\tonly three"
	badDate := "20261030141500-T3\tyesterday" + strings.Repeat("\tx", 25)
	in := filepath.Join(t.TempDir(), "bad.gkg.csv")
	if err := os.WriteFile(in, []byte(good+"\n"+short+"\n"+badDate+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "bad.parquet")
	r, err := e.Batch(t.Context(), source.GKG, false, in, out)
	if err != nil {
		t.Fatal(err)
	}
	if r.Records != 3 || r.Rows != 2 || r.Rejected() != 1 {
		t.Fatalf("result %+v, want 3 records, 2 rows, 1 rejected", r)
	}
	var nullDates int
	if err := e.DB().QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE date IS NULL`, out).Scan(&nullDates); err != nil {
		t.Fatal(err)
	}
	if nullDates != 1 {
		t.Fatalf("%d rows with a NULL date, want 1", nullDates)
	}
}

func TestCompact(t *testing.T) {
	e := newEngine(t)
	dir := t.TempDir()
	var inputs []string
	var want int64
	for _, stream := range []string{"en", "tr"} {
		out := filepath.Join(dir, stream+".parquet")
		r, err := e.Batch(t.Context(), source.GKG, stream == "tr", fixture(t, stream+".gkg.csv"), out)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, out)
		want += r.Rows
	}
	day := filepath.Join(dir, "day.parquet")
	n, err := e.Compact(t.Context(), inputs, day)
	if err != nil || n != want {
		t.Fatalf("compacted %d rows, %v; want %d", n, err, want)
	}
	var tr int64
	if err := e.DB().QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE translated`, day).Scan(&tr); err != nil || tr != 15 {
		t.Fatalf("%d translated rows in the day file, %v; want 15", tr, err)
	}
}
