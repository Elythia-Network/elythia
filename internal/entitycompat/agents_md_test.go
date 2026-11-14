package entitycompat

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAgentsMDIsASymlinkToClaudeMD: AGENTS.md is tracked as a symbolic link
// to CLAUDE.md, so coding agents that read AGENTS.md get the same rules
// (#4167).
//
// 普通のファイルに置き換わると、2つのファイルの中身が別々に直されて食い違い始める。
// 作業ツリーではなくindex(gitが記録したmodeと中身)を見るのは、symlinkを扱えない
// 環境(core.symlinks=false)でも同じ結果になるようにするため。
func TestAgentsMDIsASymlinkToClaudeMD(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-s", "--", "AGENTS.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 4 {
		t.Fatalf("AGENTS.md が追跡されていない (git ls-files -s の出力: %q)", out)
	}
	if fields[0] != "120000" {
		t.Fatalf("AGENTS.md の mode が %s。CLAUDE.md へのシンボリックリンク (120000) にする", fields[0])
	}
	target, err := exec.Command("git", "-C", root, "cat-file", "-p", fields[1]).Output()
	if err != nil {
		t.Fatalf("git cat-file: %v", err)
	}
	if string(target) != "CLAUDE.md" {
		t.Fatalf("AGENTS.md の指す先が %q。CLAUDE.md を指すようにする", target)
	}
}
