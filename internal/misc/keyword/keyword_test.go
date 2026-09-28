package keyword

import "testing"

func TestIsKeyWordIncluded(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		keywords []string
		want     bool
	}{
		{"empty keywords", "hello world", nil, false},
		{"empty text", "", []string{"hello"}, false},
		{"simple substring hit", "hello world", []string{"hello"}, true},
		{"simple substring miss", "hello world", []string{"goodbye"}, false},
		{"AND match within one filter (hit)", "hello world", []string{"hello world"}, true},
		{"AND match within one filter (partial = miss)", "hello earth", []string{"hello world"}, false},
		{"AND words can be in any order", "world hello", []string{"hello world"}, true},
		{"multiple filters, OR semantics", "spam content", []string{"good", "spam"}, true},
		{"regex hit (case-insensitive)", "Hello WORLD", []string{`/hello.*world/i`}, true},
		{"regex miss without flag", "Hello world", []string{`/hello/`}, false},
		{"regex hit without flags", "hello world", []string{`/hello/`}, true},
		{"regex multi-line flag", "line1\nline2", []string{`/^line2/m`}, true},
		{"regex dotall flag", "a\nb", []string{`/a.b/s`}, true},
		{"regex JS-only flag g is ignored (no crash)", "abcabc", []string{`/abc/g`}, true},
		{"malformed regex returns false, not panic", "any text", []string{`/[unclosed/`}, false},
		{"mix: regex miss + AND hit", "lorem ipsum", []string{`/foo/`, "lorem ipsum"}, true},
		{"empty filter is skipped", "any text", []string{"", "lorem"}, false},
		{"empty filter does NOT vacuously match", "any text", []string{""}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsKeyWordIncluded(c.text, c.keywords)
			if got != c.want {
				t.Errorf("IsKeyWordIncluded(%q, %v) = %v, want %v",
					c.text, c.keywords, got, c.want)
			}
		})
	}
}

// Matcher は IsKeyWordIncluded と同じ判定をする (同じ入力で結果が一致する)。
func TestMatcher_AgreesWithIsKeyWordIncluded(t *testing.T) {
	filters := []string{"hello world", "/fo+/i", "", "   ", "spam"}
	m, err := Compile(filters)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", "hello there world", "hello", "FOOO", "f", "spammer", "nothing"} {
		if got, want := m.Match(text), IsKeyWordIncluded(text, filters); got != want {
			t.Errorf("Match(%q) = %v, IsKeyWordIncluded = %v", text, got, want)
		}
	}
	if m.Empty() {
		t.Error("matcher with filters must not be empty")
	}
}

// 不正な regex は保存時に弾けるようエラーにする (IsKeyWordIncluded は黙って外す)。
func TestCompile_RejectsInvalidRegex(t *testing.T) {
	if _, err := Compile([]string{"/(unclosed/"}); err == nil {
		t.Fatal("want error for invalid regex")
	}
	m, err := Compile(nil)
	if err != nil || !m.Empty() || m.Match("x") {
		t.Fatalf("empty matcher: %v %v", m, err)
	}
	var nilM *Matcher
	if !nilM.Empty() || nilM.Match("x") {
		t.Fatal("nil matcher never matches")
	}
}
