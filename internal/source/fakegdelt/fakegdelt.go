// Package fakegdelt is a stand-in for GDELT's file server, for tests: file lists, zipped batches, range requests,
// and the faults the real one has (unpublished files, corrupt files, server errors).
package fakegdelt

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fault is something wrong with one file.
type Fault int

const (
	None      Fault = iota
	Missing         // listed, but the server answers 404
	Corrupt         // served with content that doesn't match the listed MD5
	Flaky           // the first two requests get HTTP 500, then it's served normally
	BadZip          // the listed size and MD5 match, but the bytes aren't a zip
	Unlisted        // served, but not in any list
	FlakyOnce       // the first request gets HTTP 500
	Truncated       // served shorter than listed
)

type file struct {
	body       []byte
	size       int64
	md5        string
	fault      Fault
	failsLeft  int
	listedLine string
}

// Server is a fake GDELT file server, an http.Handler. All methods are safe for concurrent use.
type Server struct {
	url      string
	mu       sync.Mutex
	files    map[string]*file
	order    map[string][]string // stream key ("en"/"tr") -> file names in publication order
	latest   map[string][]string // stream key -> names in the latest batch
	requests map[string]int
}

// NewHandler returns a fake server that isn't listening anywhere yet, for serving with net/http.
func NewHandler() *Server {
	return &Server{files: map[string]*file{}, order: map[string][]string{}, latest: map[string][]string{},
		requests: map[string]int{}}
}

// New starts a fake server for a test; it stops when the test ends.
func New(t testing.TB) *Server {
	s := NewHandler()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	s.url = ts.URL
	return s
}

// BaseURL is the server's equivalent of GDELT's ".../gdeltv2/" (for servers started with New).
func (s *Server) BaseURL() string { return s.url + "/gdeltv2/" }

// Name returns GDELT's file name for a batch file. stream is "en" or "tr"; feed is "events", "mentions" or "gkg".
func Name(stream, feed string, batch time.Time) string {
	infix := ""
	if stream == "tr" {
		infix = "translation."
	}
	suffix := map[string]string{"events": "export.CSV.zip", "mentions": "mentions.CSV.zip", "gkg": "gkg.csv.zip"}[feed]
	if suffix == "" {
		panic("unknown feed " + feed)
	}
	return batch.UTC().Format("20060102150405") + "." + infix + suffix
}

// AddBatch publishes one batch: csv maps a feed to its tab-separated content (feeds left out get an empty file).
// faults maps a feed to a fault for that file.
func (s *Server) AddBatch(stream string, batch time.Time, csv map[string]string, faults map[string]Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, feed := range []string{"events", "mentions", "gkg"} {
		name := Name(stream, feed, batch)
		inner := strings.TrimSuffix(name, ".zip")
		body := zipOf(inner, csv[feed])
		f := &file{body: body, size: int64(len(body)), md5: md5hex(body), fault: faults[feed]}
		switch f.fault {
		case None, Missing, Unlisted:
		case Corrupt:
			f.body = append([]byte{}, body...)
			f.body[len(f.body)/2] ^= 0xff
		case Flaky:
			f.failsLeft = 2
		case FlakyOnce:
			f.failsLeft = 1
		case BadZip:
			f.body = []byte(strings.Repeat("not a zip ", 10))
			f.size, f.md5 = int64(len(f.body)), md5hex(f.body)
		case Truncated:
			f.body = body[:len(body)/2]
		}
		f.listedLine = fmt.Sprintf("%d %s http://data.gdeltproject.org/gdeltv2/%s", f.size, f.md5, name)
		s.files[name] = f
		if f.fault != Unlisted {
			names = append(names, name)
		}
	}
	o := s.order[stream]
	inOrder := len(o) == 0 || len(names) == 0 || o[len(o)-1][:14] <= names[0][:14]
	s.order[stream] = append(o, names...)
	if !inOrder {
		sort.SliceStable(s.order[stream], func(i, j int) bool { return s.order[stream][i][:14] < s.order[stream][j][:14] })
	}
	if l := s.latest[stream]; len(l) == 0 || l[0][:14] <= names[0][:14] {
		s.latest[stream] = names
	}
}

// Requests returns how many requests a file got.
func (s *Server) Requests(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[name]
}

// ServeHTTP answers like GDELT's file server under /gdeltv2/.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/gdeltv2/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.requests[name]++
	var body []byte
	switch name {
	case "masterfilelist.txt", "masterfilelist-translation.txt", "lastupdate.txt", "lastupdate-translation.txt":
		body = []byte(s.list(name))
	default:
		f, ok := s.files[name]
		if !ok || f.fault == Missing {
			s.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		if f.failsLeft > 0 {
			f.failsLeft--
			s.mu.Unlock()
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		body = f.body
	}
	s.mu.Unlock()
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body)) // handles Range requests
}

func (s *Server) list(name string) string {
	stream := "en"
	if strings.Contains(name, "translation") {
		stream = "tr"
	}
	names := s.order[stream]
	if strings.HasPrefix(name, "lastupdate") {
		names = s.latest[stream]
	}
	var b strings.Builder
	if strings.HasPrefix(name, "masterfilelist") {
		// The real list starts in 2015; pad it so range reads have something to cut through.
		start := time.Date(2015, 2, 18, 22, 45, 0, 0, time.UTC)
		for i := range 200 {
			fmt.Fprintf(&b, "%d %s http://data.gdeltproject.org/gdeltv2/%s\n", 100+i, strings.Repeat("0", 32),
				Name(stream, "events", start.Add(time.Duration(i)*15*time.Minute)))
		}
	}
	for _, n := range names {
		b.WriteString(s.files[n].listedLine + "\n")
	}
	return b.String()
}

func zipOf(name, content string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func md5hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}
