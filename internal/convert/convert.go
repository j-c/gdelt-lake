// Package convert turns GDELT's CSV files into Parquet with an embedded DuckDB, and compacts a day's batch files into
// one. The SQL for each feed is in sql/; the Go here cleans the input, fills in the SQL and counts rows.
package convert

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2" // registers the "duckdb" database/sql driver

	"github.com/j-c/gdelt-lake/internal/source"
)

//go:embed sql/*.sql
var sqlFiles embed.FS

// Engine is one embedded DuckDB, used for one file at a time.
type Engine struct {
	db *sql.DB
}

// Open starts DuckDB with a memory cap (e.g. "1GB"), spilling to tempDir beyond it.
func Open(memoryLimit, tempDir string) (*Engine, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // settings are per connection; one file at a time anyway
	for _, stmt := range []string{
		"SET memory_limit = " + literal(memoryLimit),
		"SET temp_directory = " + literal(tempDir),
		"SET preserve_insertion_order = false", // lets COPY stream instead of buffering to keep row order
		"SET autoinstall_known_extensions = false",
		"SET autoload_known_extensions = false", // Parquet is built in; never download anything at run time
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return &Engine{db: db}, nil
}

// Close shuts DuckDB down.
func (e *Engine) Close() error { return e.db.Close() }

// Result describes one converted file.
type Result struct {
	CleanStats
	Rows int64 // rows in the Parquet file
}

// Rejected is how many records DuckDB couldn't read (wrong number of columns, after cleaning).
func (r Result) Rejected() int64 { return r.Records - r.Rows }

// Batch converts one GDELT CSV file of the given feed to a Parquet file at outPath.
func (e *Engine) Batch(ctx context.Context, feed source.Feed, translated bool, csvPath, outPath string) (Result, error) {
	cleaned := csvPath + ".clean"
	defer os.Remove(cleaned)
	st, err := cleanFile(csvPath, cleaned, recordStart(feed))
	if err != nil {
		return Result{}, fmt.Errorf("cleaning %s: %w", csvPath, err)
	}
	tmpl, err := sqlFiles.ReadFile("sql/" + string(feed) + ".sql")
	if err != nil {
		return Result{}, err
	}
	q := strings.NewReplacer(
		"{{csv}}", escape(cleaned),
		"{{out}}", escape(outPath),
		"{{translated}}", strconv.FormatBool(translated),
	).Replace(string(tmpl))
	if _, err := e.db.ExecContext(ctx, q); err != nil {
		return Result{}, fmt.Errorf("converting %s: %w", csvPath, err)
	}
	rows, err := e.Rows(ctx, outPath)
	if err != nil {
		return Result{}, err
	}
	return Result{CleanStats: st, Rows: rows}, nil
}

// Compact merges Parquet files into one at outPath and returns its row count.
func (e *Engine) Compact(ctx context.Context, inputs []string, outPath string) (int64, error) {
	if len(inputs) == 0 {
		return 0, fmt.Errorf("nothing to compact into %s", outPath)
	}
	list := make([]string, len(inputs))
	for i, in := range inputs {
		list[i] = literal(in)
	}
	q := fmt.Sprintf("COPY (SELECT * FROM read_parquet([%s], union_by_name = true)) TO %s (FORMAT parquet, COMPRESSION zstd)",
		strings.Join(list, ", "), literal(outPath))
	if _, err := e.db.ExecContext(ctx, q); err != nil {
		return 0, fmt.Errorf("compacting into %s: %w", outPath, err)
	}
	return e.Rows(ctx, outPath)
}

// Rows counts the rows in a Parquet file (from its metadata, without reading the data).
func (e *Engine) Rows(ctx context.Context, path string) (int64, error) {
	var n int64
	err := e.db.QueryRowContext(ctx, "SELECT count(*) FROM read_parquet("+literal(path)+")").Scan(&n)
	return n, err
}

// DB is the engine's DuckDB, for tests and diagnostics.
func (e *Engine) DB() *sql.DB { return e.db }

func cleanFile(in, out string, start *regexp.Regexp) (CleanStats, error) {
	src, err := os.Open(in)
	if err != nil {
		return CleanStats{}, err
	}
	defer src.Close()
	dst, err := os.Create(out)
	if err != nil {
		return CleanStats{}, err
	}
	st, err := clean(src, dst, start)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return st, err
}

// escape makes s safe inside a single-quoted SQL string.
func escape(s string) string { return strings.ReplaceAll(s, "'", "''") }

// literal is s as a single-quoted SQL string.
func literal(s string) string { return "'" + escape(s) + "'" }
