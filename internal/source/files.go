// Package source reads GDELT 2.0's file lists and downloads its 15-minute batches.
//
// GDELT publishes, per stream, a list of every file ever (masterfilelist*.txt, about 130 MB and growing) and a list of
// the latest batch (lastupdate*.txt). Each line is "<size> <md5> <url>". The master list is only ever read from its
// end, with an HTTP range request sized to the lookback window.
package source

import (
	"fmt"
	"strings"
	"time"
)

// Stream is one of GDELT's two publication streams.
type Stream string

const (
	English      Stream = "en" // English-language sources
	Translingual Stream = "tr" // machine-translated sources in about 65 other languages
)

// Streams lists both streams in a fixed order.
var Streams = []Stream{English, Translingual}

// Feed is one of the three files in a batch.
type Feed string

const (
	Events   Feed = "events"
	Mentions Feed = "mentions"
	GKG      Feed = "gkg"
)

// Feeds lists the three feeds in a fixed order.
var Feeds = []Feed{Events, Mentions, GKG}

// BatchInterval is how often GDELT publishes a batch.
const BatchInterval = 15 * time.Minute

// File is one entry in a GDELT file list.
type File struct {
	Stream Stream
	Feed   Feed
	Batch  time.Time // the batch's timestamp (UTC), from the file name
	Size   int64
	MD5    string // lower-case hex
	Name   string // e.g. "20261031094500.translation.gkg.csv.zip"
}

func (s Stream) lists() (master, latest string) {
	switch s {
	case English:
		return "masterfilelist.txt", "lastupdate.txt"
	case Translingual:
		return "masterfilelist-translation.txt", "lastupdate-translation.txt"
	}
	panic(fmt.Sprintf("unknown stream %q", s))
}

var suffixes = map[string]Feed{"export.CSV.zip": Events, "mentions.CSV.zip": Mentions, "gkg.csv.zip": GKG}

// FileName is GDELT's name for a batch file, e.g. "20261031094500.translation.gkg.csv.zip".
func FileName(s Stream, f Feed, batch time.Time) string {
	infix := ""
	if s == Translingual {
		infix = "translation."
	}
	for suffix, feed := range suffixes {
		if feed == f {
			return batch.UTC().Format("20060102150405") + "." + infix + suffix
		}
	}
	panic(fmt.Sprintf("unknown feed %q", f))
}

// parseName reads stream, feed and batch time from a file name. ok is false for names that aren't batch files.
func parseName(name string) (Stream, Feed, time.Time, bool) {
	stamp, rest, found := strings.Cut(name, ".")
	if !found || len(stamp) != 14 {
		return "", "", time.Time{}, false
	}
	t, err := time.Parse("20060102150405", stamp)
	if err != nil {
		return "", "", time.Time{}, false
	}
	stream := English
	if r, ok := strings.CutPrefix(rest, "translation."); ok {
		stream, rest = Translingual, r
	}
	feed, ok := suffixes[rest]
	if !ok {
		return "", "", time.Time{}, false
	}
	return stream, feed, t, true
}

// parseLine reads one list line. ok is false for blank lines, malformed lines and files that aren't batch files.
func parseLine(line string) (File, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return File{}, false
	}
	var size int64
	if _, err := fmt.Sscan(fields[0], &size); err != nil || size < 0 {
		return File{}, false
	}
	md5 := strings.ToLower(fields[1])
	if len(md5) != 32 || strings.Trim(md5, "0123456789abcdef") != "" {
		return File{}, false
	}
	name := fields[2][strings.LastIndex(fields[2], "/")+1:]
	stream, feed, batch, ok := parseName(name)
	if !ok {
		return File{}, false
	}
	return File{Stream: stream, Feed: feed, Batch: batch, Size: size, MD5: md5, Name: name}, true
}

// parseList reads a list. With skipFirst, the first line is dropped: a range read usually starts mid-line.
func parseList(body string, skipFirst bool) []File {
	lines := strings.Split(body, "\n")
	if skipFirst && len(lines) > 0 {
		lines = lines[1:]
	}
	var out []File
	for _, l := range lines {
		if f, ok := parseLine(l); ok {
			out = append(out, f)
		}
	}
	return out
}
