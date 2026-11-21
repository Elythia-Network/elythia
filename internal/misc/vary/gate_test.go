package vary

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// setVary matches a Vary header being overwritten (h.Set("Vary", ...)).
var setVary = regexp.MustCompile(`\.Set\(\s*(echo\.HeaderVary|"Vary")`)

// TestNoVaryOverwrite fails when non-test Go code overwrites Vary instead of
// adding to it with Add. 上書きすると、handler より前の gzip と CORS が足した
// Accept-Encoding と Origin が消える。
func TestNoVaryOverwrite(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	root := strings.TrimSpace(string(out))
	ls := exec.Command("git", "ls-files", "--", "*.go")
	ls.Dir = root
	files, err := ls.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	callers := 0
	for _, name := range strings.Fields(string(files)) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if setVary.MatchString(line) {
				t.Errorf("%s:%d overwrites Vary; use vary.Add", name, i+1)
			}
		}
		callers += strings.Count(string(src), "vary.Add(")
	}
	// 走査が空振りしても緑にならないよう、実在する呼び出し (ActivityPub と well-known と
	// proxy の handler) を数える。
	if callers < 12 {
		t.Fatalf("found %d vary.Add callers, want at least 12 (did the scan miss the files?)", callers)
	}
}
