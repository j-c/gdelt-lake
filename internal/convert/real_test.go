package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/j-c/gdelt-lake/internal/source"
)

// TestRealFiles converts every unzipped GDELT file in $GDELT_LAKE_REAL_DIR (skipped when unset) and expects every
// record to be read. Use it to check the SQL against fresh real batches.
func TestRealFiles(t *testing.T) {
	dir := os.Getenv("GDELT_LAKE_REAL_DIR")
	if dir == "" {
		t.Skip("set GDELT_LAKE_REAL_DIR to a folder of unzipped GDELT files")
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.[Cc][Ss][Vv]"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no CSV files in %s (%v)", dir, err)
	}
	e := newEngine(t)
	for _, csv := range names {
		base := filepath.Base(csv)
		var feed source.Feed
		switch {
		case strings.Contains(base, ".export."):
			feed = source.Events
		case strings.Contains(base, ".mentions."):
			feed = source.Mentions
		case strings.Contains(base, ".gkg."):
			feed = source.GKG
		default:
			continue
		}
		r, err := e.Batch(t.Context(), feed, strings.Contains(base, ".translation."), csv, filepath.Join(t.TempDir(), base+".parquet"))
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		t.Logf("%s: %d records, %d rows, %d continuation lines, %d lines with invalid UTF-8", base, r.Records, r.Rows,
			r.ContinuationLines, r.InvalidUTF8Lines)
		if r.Rejected() != 0 {
			t.Errorf("%s: %d records rejected", base, r.Rejected())
		}
	}
}
