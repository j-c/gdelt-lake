package main

import (
	"strings"
	"testing"
)

const mod = "github.com/j-c/gdelt-lake"

// A trimmed `go test -json -cover` stream: one passing package, one with a failed subtest, one that doesn't build.
const stream = `{"Action":"start","Package":"github.com/j-c/gdelt-lake/internal/state"}
{"Action":"run","Package":"github.com/j-c/gdelt-lake/internal/state","Test":"TestKey"}
{"Action":"pass","Package":"github.com/j-c/gdelt-lake/internal/state","Test":"TestKey","Elapsed":0.01}
{"Action":"output","Package":"github.com/j-c/gdelt-lake/internal/state","Output":"coverage: 81.5% of statements\n"}
{"Action":"pass","Package":"github.com/j-c/gdelt-lake/internal/state","Elapsed":0.02}
{"Action":"run","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestRun"}
{"Action":"run","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestRun/late"}
{"Action":"output","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestRun/late","Output":"    ingest_test.go:42: summary {Done:1}, want ` + "```" + `2` + "```" + `\n"}
{"Action":"fail","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestRun/late","Elapsed":1.5}
{"Action":"fail","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestRun","Elapsed":1.6}
{"Action":"run","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestSkipped"}
{"Action":"output","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestSkipped","Output":"    ingest_test.go:7: needs <real data> | later\n"}
{"Action":"skip","Package":"github.com/j-c/gdelt-lake/internal/ingest","Test":"TestSkipped","Elapsed":0}
{"Action":"fail","Package":"github.com/j-c/gdelt-lake/internal/ingest","Elapsed":2.1}
{"ImportPath":"github.com/j-c/gdelt-lake/internal/source [github.com/j-c/gdelt-lake/internal/source.test]","Action":"build-output","Output":"internal/source/client.go:9:2: undefined: x\n"}
{"ImportPath":"github.com/j-c/gdelt-lake/internal/source [github.com/j-c/gdelt-lake/internal/source.test]","Action":"build-fail"}
{"Action":"start","Package":"github.com/j-c/gdelt-lake/internal/source"}
{"Action":"output","Package":"github.com/j-c/gdelt-lake/internal/source","Output":"FAIL\tgithub.com/j-c/gdelt-lake/internal/source [build failed]\n"}
{"Action":"fail","Package":"github.com/j-c/gdelt-lake/internal/source","Elapsed":0}
FAIL
`

func TestReport(t *testing.T) {
	rep, err := read(strings.NewReader(stream), mod)
	if err != nil {
		t.Fatal(err)
	}
	var md strings.Builder
	if rep.write(&md) {
		t.Fatal("reported success")
	}
	out := md.String()
	for _, want := range []string{
		"## ❌ Test report",
		"**1 passed, 1 failed, 1 skipped** in 3 packages",
		"4 results counting subtests",
		"| `internal/state` | ✅ pass | 1 (1) | 0.02s | 81.5% |",
		"| `internal/ingest` | ❌ fail | 2 (3) |",
		"<code>internal/ingest</code> TestRun/late (1.50s)",
		"want '''2'''", // output can't close the code block
		"<code>internal/source</code> failed outside a test",
		"undefined: x",
		// every test is listed, subtests indented, with the message of a failure
		"<details open><summary>❌ <code>internal/ingest</code>: 0 passed, 2 failed, 1 skipped</summary>",
		"| TestRun | ❌ fail | 1.60s |",
		"| &nbsp;&nbsp;↳ late | ❌ fail | 1.50s | ingest_test.go:42: summary {Done:1}, want ```2``` |",
		"| TestSkipped | ➖ skip | 0.00s | ingest_test.go:7: needs &lt;real data&gt; \\| later |",
		"<details><summary>✅ <code>internal/state</code>: 1 passed, 0 failed, 0 skipped</summary>",
		"| TestKey | ✅ pass | 0.01s |  |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "<code>internal/ingest</code> TestRun (") {
		t.Error("the parent of a failed subtest is listed as a failure of its own")
	}

	var ann strings.Builder
	rep.annotate(&ann)
	if want := "::error file=internal/ingest/ingest_test.go,line=42,title=TestRun/late::summary {Done:1}"; !strings.HasPrefix(ann.String(), want) {
		t.Errorf("annotation %q", ann.String())
	}
}

func TestAllPassing(t *testing.T) {
	ok := `{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.1}
{"Action":"pass","Package":"p","Elapsed":0.1}
`
	rep, err := read(strings.NewReader(ok), mod)
	if err != nil {
		t.Fatal(err)
	}
	var md strings.Builder
	if !rep.write(&md) || !strings.Contains(md.String(), "## ✅ Test report") || strings.Contains(md.String(), "Failures") {
		t.Fatalf("%s", md.String())
	}
}
