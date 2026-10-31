// Command lake-accept is test tooling for the container check (scripts/accept.sh); it isn't shipped in the image.
//
//	lake-accept serve [--addr :8080] [--testdata DIR]   a fake GDELT serving the test fixtures as the current batch
//	lake-accept health URL                              exit 0 if URL answers 200 (for compose healthchecks)
//	lake-accept verify [--lake DIR]                     check what an ingester run left in a lake
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/j-c/gdelt-lake/internal/convert"
	"github.com/j-c/gdelt-lake/internal/ingest"
	"github.com/j-c/gdelt-lake/internal/source"
	"github.com/j-c/gdelt-lake/internal/source/fakegdelt"
	"github.com/j-c/gdelt-lake/internal/state"
	"github.com/j-c/gdelt-lake/internal/storage"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: lake-accept serve|health|verify [options]"))
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "health":
		err = health(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "lake-accept:", err)
	os.Exit(1)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	testdata := fs.String("testdata", "testdata", "folder with the fixture CSV files")
	_ = fs.Parse(args)
	srv := fakegdelt.NewHandler()
	batch := time.Now().UTC().Truncate(source.BatchInterval)
	for _, s := range []string{"en", "tr"} {
		csv := map[string]string{}
		for _, f := range []string{"events", "mentions", "gkg"} {
			b, err := os.ReadFile(filepath.Join(*testdata, s+"."+f+".csv"))
			if err != nil {
				return err
			}
			csv[f] = string(b)
		}
		srv.AddBatch(s, batch, csv, nil)
	}
	fmt.Printf("fake GDELT on %s, serving batch %s\n", *addr, batch.Format(time.RFC3339))
	return (&http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe()
}

func health(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lake-accept health URL")
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(args[0])
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", args[0], resp.StatusCode)
	}
	return nil
}

// verify checks a lake after one run against "serve": one batch in both streams, every file done, every record read,
// and each Parquet file holding the rows its state says.
func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	root := fs.String("lake", "/lake", "the lake's folder")
	_ = fs.Parse(args)
	ctx := context.Background()
	lake, err := storage.NewLocal(*root)
	if err != nil {
		return err
	}
	info, ok, err := state.LoadLake(ctx, lake)
	if err != nil || !ok {
		return fmt.Errorf("no _state/lake.json (%v)", err)
	}
	day, err := state.Load(ctx, lake, state.DateOf(info.Started))
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	eng, err := convert.Open("256MB", tmp)
	if err != nil {
		return err
	}
	defer eng.Close()
	var done int
	for _, s := range source.Streams {
		for _, f := range source.Feeds {
			key := state.Key(s, f, info.Started)
			st := day.Files[key]
			if st == nil || st.Status != state.Done {
				b, _ := json.Marshal(st)
				return fmt.Errorf("%s: %s, want done", key, b)
			}
			if st.Rows == 0 || st.Rows != st.Records || st.Rejected != 0 {
				return fmt.Errorf("%s: %d records, %d rows, %d rejected", key, st.Records, st.Rows, st.Rejected)
			}
			p := filepath.Join(*root, filepath.FromSlash(ingest.LakePath(s, f, info.Started)))
			n, err := eng.Rows(ctx, p)
			if err != nil {
				return err
			}
			if n != st.Rows {
				return fmt.Errorf("%s: %d rows in the file, state says %d", key, n, st.Rows)
			}
			done++
			fmt.Printf("ok  %-28s %4d rows\n", strings.TrimSuffix(filepath.Base(p), ".parquet")+" "+string(f), n)
		}
	}
	fmt.Printf("verified %d files\n", done)
	return nil
}
