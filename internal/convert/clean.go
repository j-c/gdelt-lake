package convert

import (
	"bufio"
	"bytes"
	"io"
	"regexp"
	"unicode/utf8"

	"github.com/j-c/gdelt-lake/internal/source"
)

// CleanStats counts what clean changed.
type CleanStats struct {
	Records           int64 // records written
	ContinuationLines int64 // lines joined onto the record before them (a field contained a line break)
	InvalidUTF8Lines  int64 // lines whose invalid UTF-8 bytes were replaced with U+FFFD
}

// recordStart matches the first bytes of a record in each feed. GDELT doesn't escape line breaks inside fields
// (they show up in GKG's last column, Extras), so a line that doesn't start like a record continues the one before.
func recordStart(f source.Feed) *regexp.Regexp {
	switch f {
	case source.Events:
		return regexp.MustCompile(`^\d+\t\d{8}\t`) // GLOBALEVENTID, SQLDATE
	case source.Mentions:
		return regexp.MustCompile(`^\d+\t\d{14}\t\d{14}\t`) // GLOBALEVENTID, EventTimeDate, MentionTimeDate
	case source.GKG:
		return regexp.MustCompile(`^\d{14}-T?\d+\t`) // GKGRECORDID
	}
	panic("unknown feed " + string(f))
}

// clean copies a GDELT CSV file from in to out with one record per line and valid UTF-8, so DuckDB can read every
// record: continuation lines are joined onto their record with a space, and invalid bytes become U+FFFD.
func clean(in io.Reader, out io.Writer, start *regexp.Regexp) (CleanStats, error) {
	var st CleanStats
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriterSize(out, 1<<20)
	var record []byte
	flush := func() error {
		if record == nil {
			return nil
		}
		st.Records++
		if _, err := w.Write(record); err != nil {
			return err
		}
		record = nil
		return w.WriteByte('\n')
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return st, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			if !utf8.Valid(line) {
				st.InvalidUTF8Lines++
				line = bytes.ToValidUTF8(line, []byte("\uFFFD"))
			}
			if record != nil && !start.Match(line) {
				st.ContinuationLines++
				record = append(append(record, ' '), line...)
			} else {
				if ferr := flush(); ferr != nil {
					return st, ferr
				}
				record = append([]byte(nil), line...)
			}
		}
		if err == io.EOF {
			break
		}
	}
	if err := flush(); err != nil {
		return st, err
	}
	return st, w.Flush()
}
