// Command testreport turns `go test -json` output into a Markdown test report, and GitHub annotations for failed
// tests. CI writes the report to the run's summary page:
//
//	go test -json -cover ./... > test.json
//	go run ./scripts/testreport -annotate < test.json >> "$GITHUB_STEP_SUMMARY"
//
// It exits 1 if any package or test failed, so it can also be used on its own.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// event is one line of `go test -json` (see `go doc test2json`).
type event struct {
	Action     string
	Package    string
	ImportPath string // build-output and build-fail events
	Test       string
	Elapsed    float64
	Output     string
}

type test struct {
	pkg, name string
	result    string // pass, fail, skip
	elapsed   time.Duration
	output    strings.Builder
}

type pkg struct {
	name     string
	result   string
	elapsed  time.Duration
	coverage string
	output   strings.Builder // package-level output (panics, timeouts, build errors)
}

type report struct {
	module string
	pkgs   map[string]*pkg
	tests  map[string]*test // by package + "\x00" + test name
}

var coverageRE = regexp.MustCompile(`coverage: ([0-9.]+% of statements)`)

func read(r io.Reader, module string) (*report, error) {
	rep := &report{module: module, pkgs: map[string]*pkg{}, tests: map[string]*test{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // go test prints some lines that aren't JSON (e.g. "FAIL" summaries); skip them
		}
		name := e.Package
		if name == "" {
			name = e.ImportPath
		}
		if name == "" {
			continue
		}
		if i := strings.IndexByte(name, ' '); i > 0 { // "pkg [pkg.test]" in build events
			name = name[:i]
		}
		p := rep.pkgs[name]
		if p == nil {
			p = &pkg{name: name}
			rep.pkgs[name] = p
		}
		switch {
		case e.Action == "build-output":
			p.output.WriteString(e.Output)
		case e.Action == "build-fail":
			p.result = "fail"
		case e.Test == "":
			switch e.Action {
			case "output":
				if m := coverageRE.FindStringSubmatch(e.Output); m != nil {
					p.coverage = m[1]
				}
				p.output.WriteString(e.Output)
			case "pass", "fail", "skip":
				p.result = e.Action
				p.elapsed = seconds(e.Elapsed)
			}
		default:
			key := name + "\x00" + e.Test
			t := rep.tests[key]
			if t == nil {
				t = &test{pkg: name, name: e.Test}
				rep.tests[key] = t
			}
			switch e.Action {
			case "output":
				t.output.WriteString(e.Output)
			case "pass", "fail", "skip":
				t.result = e.Action
				t.elapsed = seconds(e.Elapsed)
			}
		}
	}
	return rep, sc.Err()
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// failed returns the failed tests that have no failed subtests (the parents of a failed subtest fail too).
func (r *report) failed() []*test {
	var out []*test
	for _, t := range r.tests {
		if t.result != "fail" {
			continue
		}
		leaf := true
		for _, u := range r.tests {
			if u.pkg == t.pkg && u.result == "fail" && strings.HasPrefix(u.name, t.name+"/") {
				leaf = false
				break
			}
		}
		if leaf {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pkg+out[i].name < out[j].pkg+out[j].name })
	return out
}

func (r *report) short(p string) string {
	if s := strings.TrimPrefix(p, r.module+"/"); s != p {
		return s
	}
	return p
}

func (r *report) write(w io.Writer) (ok bool) {
	var pass, fail, skip int
	var topLevel []*test
	for _, t := range r.tests {
		if strings.Contains(t.name, "/") {
			continue // totals count top-level tests; subtests show up under their failures
		}
		topLevel = append(topLevel, t)
		switch t.result {
		case "pass":
			pass++
		case "fail":
			fail++
		case "skip":
			skip++
		}
	}
	var names []string
	var total time.Duration
	failedPkgs := 0
	for n, p := range r.pkgs {
		names = append(names, n)
		total += p.elapsed
		if p.result == "fail" {
			failedPkgs++
		}
	}
	sort.Strings(names)
	ok = fail == 0 && failedPkgs == 0

	icon := "✅"
	if !ok {
		icon = "❌"
	}
	fmt.Fprintf(w, "## %s Test report\n\n", icon)
	fmt.Fprintf(w, "**%d passed, %d failed, %d skipped** in %d packages (%s of package time).\n\n",
		pass, fail, skip, len(names), total.Round(time.Second))

	fmt.Fprintln(w, "| Package | Result | Tests | Time | Coverage |")
	fmt.Fprintln(w, "|---|---|---:|---:|---:|")
	for _, n := range names {
		p := r.pkgs[n]
		count := 0
		for _, t := range topLevel {
			if t.pkg == n {
				count++
			}
		}
		result := map[string]string{"pass": "✅ pass", "fail": "❌ fail", "skip": "➖ no tests", "": "❔ unknown"}[p.result]
		if p.result == "pass" && count == 0 {
			result = "➖ no tests"
		}
		cov := p.coverage
		if cov == "" {
			cov = "–"
		}
		fmt.Fprintf(w, "| `%s` | %s | %d | %s | %s |\n", r.short(n), result, count, p.elapsed.Round(10*time.Millisecond),
			strings.TrimSuffix(cov, " of statements"))
	}

	if failed := r.failed(); len(failed) > 0 {
		fmt.Fprint(w, "\n### Failures\n\n")
		for _, t := range failed {
			fmt.Fprintf(w, "<details open><summary><code>%s</code> %s (%s)</summary>\n\n```\n%s```\n\n</details>\n\n",
				r.short(t.pkg), t.name, t.elapsed.Round(10*time.Millisecond), fence(t.output.String()))
		}
	}
	for _, n := range names { // packages that failed without a failed test: build errors, panics, timeouts
		p := r.pkgs[n]
		if p.result != "fail" || slices.ContainsFunc(r.failed(), func(t *test) bool { return t.pkg == n }) {
			continue
		}
		fmt.Fprintf(w, "\n<details open><summary><code>%s</code> failed outside a test</summary>\n\n```\n%s```\n\n</details>\n",
			r.short(n), fence(tail(p.output.String(), 60)))
	}

	sort.Slice(topLevel, func(i, j int) bool { return topLevel[i].elapsed > topLevel[j].elapsed })
	if len(topLevel) > 10 {
		topLevel = topLevel[:10]
	}
	if len(topLevel) > 0 {
		fmt.Fprint(w, "\n<details><summary>Slowest tests</summary>\n\n| Test | Package | Time |\n|---|---|---:|\n")
		for _, t := range topLevel {
			fmt.Fprintf(w, "| %s | `%s` | %s |\n", t.name, r.short(t.pkg), t.elapsed.Round(10*time.Millisecond))
		}
		fmt.Fprint(w, "\n</details>\n")
	}
	return ok
}

// annotate prints GitHub workflow commands that mark failed tests on the run page and in the diff.
func (r *report) annotate(w io.Writer) {
	loc := regexp.MustCompile(`^\s+([\w.-]+_test\.go):(\d+): (.*)$`)
	for _, t := range r.failed() {
		file, line, msg := "", "", strings.TrimSpace(tail(t.output.String(), 20))
		for _, l := range strings.Split(t.output.String(), "\n") {
			if m := loc.FindStringSubmatch(l); m != nil {
				file, line, msg = path.Join(r.short(t.pkg), m[1]), m[2], m[3]
				break
			}
		}
		props := "title=" + escapeProp(t.name)
		if file != "" {
			props = "file=" + escapeProp(file) + ",line=" + line + "," + props
		}
		fmt.Fprintf(w, "::error %s::%s\n", props, escapeData(msg))
	}
}

func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func escapeProp(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

// fence keeps output from closing the Markdown code block it's in.
func fence(s string) string {
	s = strings.ReplaceAll(s, "```", "'''")
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

func tail(s string, lines int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > lines {
		l = l[len(l)-lines:]
	}
	return strings.Join(l, "\n") + "\n"
}

func main() {
	module := flag.String("module", "github.com/j-c/gdelt-lake", "module path, trimmed from package names")
	annotate := flag.Bool("annotate", false, "also print GitHub annotations for failed tests (to stderr)")
	flag.Parse()
	rep, err := read(os.Stdin, *module)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testreport:", err)
		os.Exit(2)
	}
	if len(rep.pkgs) == 0 {
		fmt.Fprintln(os.Stderr, "testreport: no `go test -json` events on stdin")
		os.Exit(2)
	}
	ok := rep.write(os.Stdout)
	if *annotate {
		rep.annotate(os.Stderr) // the runner reads workflow commands from either stream
	}
	if !ok {
		os.Exit(1)
	}
}
