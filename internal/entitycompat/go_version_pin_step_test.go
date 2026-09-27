package entitycompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	ciWorkflow         = ".github/workflows/ci.yml"
	goVersionPinStepNm = "Go version pin is consistent between go.mod and Dockerfiles"
)

// TestGoVersionPinStepAcceptsDigest runs the vulncheck job's Go version check
// from ci.yml against synthetic repositories.
//
// **配る Dockerfile は base image を `tag@sha256:<digest>` で固定している。**
// 以前の判定は image 全体を `golang:<go.mod の版>-alpine` と文字列比較していた
// ので、digest を併記した瞬間に正しい pin が不合格になった。tag 側で版を照合し、
// digest は形だけ見る (tag との対応は registry に問い合わせないと分からない)。
func TestGoVersionPinStepAcceptsDigest(t *testing.T) {
	bash, err := exec.LookPath("bash")
	require.NoError(t, err, "bash が要る")

	wf := parseWorkflow(t, readRepoFile(t, ciWorkflow))
	var run string
	for _, s := range wf.steps {
		if s.Name == goVersionPinStepNm {
			run = s.Run
		}
	}
	require.NotEmptyf(t, run, "%s に step %q が無い", ciWorkflow, goVersionPinStepNm)

	digest := "sha256:" + strings.Repeat("0a", 32)
	tests := []struct {
		name   string
		from   string
		wantOK bool
	}{
		{"tag only", "FROM golang:1.27.1-alpine AS builder", true},
		{"tag with digest", "FROM golang:1.27.1-alpine@" + digest + " AS builder", true},
		{"platform flag with digest", "FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@" + digest + " AS builder", true},
		{"older patch", "FROM golang:1.27.0-alpine AS builder", false},
		{"older patch with digest", "FROM golang:1.27.0-alpine@" + digest + " AS builder", false},
		{"floating tag", "FROM golang:1.27-alpine AS builder", false},
		{"malformed digest", "FROM golang:1.27.1-alpine@sha256:abcd AS builder", false},
		{"digest without tag", "FROM golang@" + digest + " AS builder", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.27.1\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(tt.from+"\n"), 0o644))
			env := filterGitEnv(os.Environ())
			for _, args := range [][]string{{"init", "-q"}, {"add", "go.mod", "Dockerfile"}} {
				c := exec.Command("git", args...)
				c.Dir = dir
				c.Env = env
				out, err := c.CombinedOutput()
				require.NoErrorf(t, err, "git %v: %s", args, out)
			}

			cmd := exec.Command(bash, "-c", run)
			cmd.Dir = dir
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if tt.wantOK {
				require.NoErrorf(t, err, "step は通るべき:\n%s", out)
			} else {
				require.Errorf(t, err, "step は落ちるべき:\n%s", out)
			}
		})
	}
}
