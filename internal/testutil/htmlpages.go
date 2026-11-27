package testutil

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// minHTMLPages is the fewest pages HTMLPagesMatchGolden accepts, so that a scan
// that found nothing does not pass.
const minHTMLPages = 25

// HTMLPagesMatchGolden checks a function that reads pages from outside against
// the values the same function gave when it used x/net/html's tree.
//
// The pages are internal/misc/htmlstream/testdata/pages/*.html. golden is a
// JSON object from page name to the value of legacy (the function as it was,
// kept in the caller's tests); with update set, it is written from legacy
// first. Each page's value of current must equal the golden one, except for
// the pages in differences, whose value of current must equal the listed one
// (and differ from the golden one).
//
// 外から来る HTML を読む箇所 (URL のプレビュー、アイコン、rel=me、OAuth の
// クライアント) は、x/net/html の木 (html.Parse) をやめたときに、同じ値を返すことを
// ここで確かめる。golden は前の実装から作り、作り方は -update-golden で再現できる。
func HTMLPagesMatchGolden[T any](t *testing.T, golden string, update bool, legacy, current func(page []byte) T, differences map[string]T) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(projectRoot(), "internal", "misc", "htmlstream", "testdata", "pages", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < minHTMLPages {
		t.Fatalf("found %d pages, want at least %d", len(files), minHTMLPages)
	}
	pages := map[string][]byte{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		pages[strings.TrimSuffix(filepath.Base(f), ".html")] = body
	}
	if update {
		want := map[string]T{}
		for name, body := range pages {
			want[name] = legacy(body)
		}
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(want); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(pages))
	for name := range pages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, ok := want[name]
		if !ok {
			t.Errorf("%s: no golden value (run with -update-golden)", name)
			continue
		}
		got, err := json.Marshal(current(pages[name]))
		if err != nil {
			t.Fatal(err)
		}
		if d, ok := differences[name]; ok {
			dj, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if jsonEqual(t, w, got) {
				t.Errorf("%s: no longer differs from the golden value; remove it from the differences", name)
			}
			w = dj
		}
		if !jsonEqual(t, w, got) {
			t.Errorf("%s: got %s, want %s", name, got, w)
		}
	}
	for name := range want {
		if _, ok := pages[name]; !ok {
			t.Errorf("golden has a value for a missing page: %s", name)
		}
	}
	for name := range differences {
		if _, ok := pages[name]; !ok {
			t.Errorf("differences has an entry for a missing page: %s", name)
		}
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	xj, _ := json.Marshal(x)
	yj, _ := json.Marshal(y)
	return bytes.Equal(xj, yj)
}
