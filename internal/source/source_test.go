package source

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/j-c/gdelt-lake/internal/source/fakegdelt"
)

func testClient(base string) *Client {
	c := NewClient(base, "gdelt-lake-test")
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestParseLine(t *testing.T) {
	f, ok := parseLine("8030555 24641c5bdd216d3f67dd837a0dd85f69 http://data.gdeltproject.org/gdeltv2/20261031093000.translation.gkg.csv.zip")
	want := File{Stream: Translingual, Feed: GKG, Batch: time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC), Size: 8030555,
		MD5: "24641c5bdd216d3f67dd837a0dd85f69", Name: "20261031093000.translation.gkg.csv.zip"}
	if !ok || f != want {
		t.Fatalf("got %+v, %v", f, ok)
	}
	f, ok = parseLine("56980 EF7F8E41F8860FAF039236C0DB27E9F8 http://data.gdeltproject.org/gdeltv2/20261031094500.export.CSV.zip")
	if !ok || f.Stream != English || f.Feed != Events || f.MD5 != "ef7f8e41f8860faf039236c0db27e9f8" {
		t.Fatalf("got %+v, %v", f, ok)
	}
	for _, bad := range []string{
		"", "161890667bb211e4 http://data.gdeltproject.org/gdeltv2/20261031093000.gkg.csv.zip", // cut by a range read
		"12 nothex0000000000000000000000000 http://x/20261031093000.gkg.csv.zip",
		"12 24641c5bdd216d3f67dd837a0dd85f69 http://x/20261031093000.something.else.zip",
		"12 24641c5bdd216d3f67dd837a0dd85f69 http://x/2026103109.gkg.csv.zip",
		"-1 24641c5bdd216d3f67dd837a0dd85f69 http://x/20261031093000.gkg.csv.zip",
	} {
		if f, ok := parseLine(bad); ok {
			t.Errorf("parseLine(%q) = %+v, want not ok", bad, f)
		}
	}
}

// day publishes a full day of batches in both streams on the fake server.
func day(srv *fakegdelt.Server, d time.Time) {
	for i := range 96 {
		b := d.Add(time.Duration(i) * BatchInterval)
		for _, s := range []string{"en", "tr"} {
			srv.AddBatch(s, b, map[string]string{"events": "1\t2\n"}, nil)
		}
	}
}

func TestLatest(t *testing.T) {
	srv := fakegdelt.New(t)
	day(srv, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	c := testClient(srv.BaseURL())
	for _, s := range Streams {
		files, err := c.Latest(t.Context(), s)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 3 {
			t.Fatalf("%s: %d files", s, len(files))
		}
		for _, f := range files {
			if f.Stream != s || !f.Batch.Equal(time.Date(2026, 10, 30, 23, 45, 0, 0, time.UTC)) {
				t.Errorf("%s: unexpected latest file %+v", s, f)
			}
		}
	}
}

func TestSince(t *testing.T) {
	srv := fakegdelt.New(t)
	day(srv, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	c := testClient(srv.BaseURL())
	since := time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC)
	files, err := c.Since(t.Context(), Translingual, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 24*3 { // 18:00 to 23:45 is 24 batches
		t.Fatalf("%d files, want 72", len(files))
	}
	for _, f := range files {
		if f.Stream != Translingual || f.Batch.Before(since) {
			t.Fatalf("unexpected file %+v", f)
		}
	}
	// Asking for more than the list holds returns everything (the padding is from 2015, before "since").
	all, err := c.Since(t.Context(), English, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))
	if err != nil || len(all) != 96*3 {
		t.Fatalf("%d files, %v; want 288", len(all), err)
	}
}

func TestSinceGrowsTheRangeUntilItReachesBack(t *testing.T) {
	srv := fakegdelt.New(t)
	// Ten days of batches starting yesterday, asked for from the first: the first range read is sized for one day
	// ("now - since"), so the client has to keep doubling it to reach back.
	start := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	for d := range 10 {
		day(srv, start.AddDate(0, 0, d))
	}
	c := testClient(srv.BaseURL())
	files, err := c.Since(t.Context(), English, start)
	if err != nil || len(files) != 10*96*3 {
		t.Fatalf("%d files, %v; want %d", len(files), err, 10*96*3)
	}
	if n := srv.Requests("masterfilelist.txt"); n < 2 {
		t.Fatalf("master list read %d times; the test needs the first range to fall short", n)
	}
}

func TestDownload(t *testing.T) {
	srv := fakegdelt.New(t)
	b := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	srv.AddBatch("en", b, map[string]string{"events": "a\tb\n", "mentions": "c\n", "gkg": "d\n"}, nil)
	b2 := b.Add(BatchInterval)
	srv.AddBatch("en", b2, nil, map[string]fakegdelt.Fault{"events": fakegdelt.Missing, "mentions": fakegdelt.Corrupt,
		"gkg": fakegdelt.Flaky})
	b3 := b2.Add(BatchInterval)
	srv.AddBatch("en", b3, nil, map[string]fakegdelt.Fault{"events": fakegdelt.Truncated, "gkg": fakegdelt.BadZip})
	c := testClient(srv.BaseURL())
	files, err := c.Since(t.Context(), English, b)
	if err != nil || len(files) != 9 {
		t.Fatalf("%d files, %v", len(files), err)
	}
	byName := map[string]File{}
	for _, f := range files {
		byName[f.Name] = f
	}
	dir := t.TempDir()
	get := func(stream, feed string, batch time.Time) (string, error) {
		return c.Download(t.Context(), byName[fakegdelt.Name(stream, feed, batch)], dir)
	}

	zipPath, err := get("en", "events", b)
	if err != nil {
		t.Fatal(err)
	}
	csv, err := Unzip(zipPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(csv); string(body) != "a\tb\n" {
		t.Fatalf("unzipped %q", body)
	}

	if _, err := get("en", "events", b2); !errors.Is(err, ErrMissing) {
		t.Errorf("missing file: %v", err)
	}
	if n := srv.Requests(fakegdelt.Name("en", "events", b2)); n != 1 {
		t.Errorf("missing file asked %d times, want 1 (no retries for 404)", n)
	}
	if _, err := get("en", "mentions", b2); !errors.Is(err, ErrCorrupt) {
		t.Errorf("corrupt file: %v", err)
	}
	if n := srv.Requests(fakegdelt.Name("en", "mentions", b2)); n != 4 {
		t.Errorf("corrupt file asked %d times, want 4 (retried)", n)
	}
	if _, err := get("en", "gkg", b2); err != nil {
		t.Errorf("flaky file: %v", err)
	}
	if _, err := get("en", "events", b3); !errors.Is(err, ErrCorrupt) {
		t.Errorf("truncated file: %v", err)
	}
	badZip, err := get("en", "gkg", b3)
	if err != nil {
		t.Fatalf("bad zip should download (size and MD5 match): %v", err)
	}
	if _, err := Unzip(badZip, dir); !errors.Is(err, ErrCorrupt) {
		t.Errorf("bad zip: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 4 { // the good zip and its CSV, the flaky one, the bad zip
		t.Errorf("%d files left in the download dir, want 4 (failed downloads removed)", len(left))
	}
}

func TestDownloadStopsWhenCancelled(t *testing.T) {
	srv := fakegdelt.New(t)
	b := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	srv.AddBatch("en", b, nil, map[string]fakegdelt.Fault{"events": fakegdelt.Flaky})
	c := NewClient(srv.BaseURL(), "test") // real sleep, with a 5 s backoff
	files, err := c.Latest(t.Context(), English)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Download(ctx, files[0], t.TempDir()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want the context's", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Download kept waiting after the context ended")
	}
}
