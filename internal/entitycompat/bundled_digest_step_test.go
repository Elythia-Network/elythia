package entitycompat

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	dockerWorkflow         = ".github/workflows/docker.yml"
	bundledDigestStepName  = "Assets image digest matches its tag"
	bundledDigestStubBytes = "stub-manifest\n"
)

// TestBundledDigestStepAllowsPastTagRepublish runs the digest check step of
// docker.yml against synthetic Dockerfiles.
//
// **過去タグの再 publish (`-f tag=1.1.1`) を塞がない。** digest の併記は後から
// 入ったので、既存リリースタグの Dockerfile.bundled は tag だけを持つ。そこで
// 落とすと、docker.yml 冒頭の「取りこぼしはこちらで拾う」経路が bundled だけ
// 使えなくなる。一方で tag input の無い (= 現在のブランチを焼く) 実行で digest が
// 無いのは pin の付け忘れなので落とす。digest があるときは tag input の有無に
// 関わらず照合する。
//
// step の `run:` を bash でそのまま実行し、`docker` は固定の manifest を返す stub に
// 差し替える。
func TestBundledDigestStepAllowsPastTagRepublish(t *testing.T) {
	bash, err := exec.LookPath("bash")
	require.NoError(t, err, "bash が要る")

	wf := parseWorkflow(t, readRepoFile(t, dockerWorkflow))
	var step *workflowStep
	for i := range wf.steps {
		if wf.steps[i].Name == bundledDigestStepName {
			step = &wf.steps[i]
		}
	}
	require.NotNilf(t, step, "%s に step %q が無い", dockerWorkflow, bundledDigestStepName)
	assert.Containsf(t, step.scalars, "${{ inputs.tag }}",
		"step %q は INPUT_TAG に inputs.tag を渡すこと", bundledDigestStepName)

	sum := sha256.Sum256([]byte(bundledDigestStubBytes))
	good := "sha256:" + hex.EncodeToString(sum[:])

	tests := []struct {
		name     string
		dockerfn string
		inputTag string
		wantOK   bool
	}{
		{"past tag without digest", "ARG MISSKEY_ASSETS_IMAGE=ghcr.io/x/assets:1.0\n", "1.1.1", true},
		{"past tag without the arg", "FROM ghcr.io/x/assets:1.0 AS assets\n", "1.1.1", true},
		{"branch without digest", "ARG MISSKEY_ASSETS_IMAGE=ghcr.io/x/assets:1.0\n", "", false},
		{"branch without the arg", "FROM ghcr.io/x/assets:1.0 AS assets\n", "", false},
		{"branch with matching digest", "ARG MISSKEY_ASSETS_IMAGE=ghcr.io/x/assets:1.0@" + good + "\n", "", true},
		{"branch with stale digest", "ARG MISSKEY_ASSETS_IMAGE=ghcr.io/x/assets:1.0@sha256:" + strings.Repeat("0", 64) + "\n", "", false},
		{"past tag with stale digest", "ARG MISSKEY_ASSETS_IMAGE=ghcr.io/x/assets:1.0@sha256:" + strings.Repeat("0", 64) + "\n", "1.1.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			require.NoError(t, os.Mkdir(bin, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"),
				[]byte("#!/bin/sh\nprintf '"+strings.TrimSuffix(bundledDigestStubBytes, "\n")+"\\n'\n"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile.bundled"), []byte(tt.dockerfn), 0o644))

			cmd := exec.Command(bash, "-c", step.Run)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"INPUT_TAG="+tt.inputTag)
			out, err := cmd.CombinedOutput()
			if tt.wantOK {
				require.NoErrorf(t, err, "step は通るべき:\n%s", out)
			} else {
				require.Errorf(t, err, "step は落ちるべき:\n%s", out)
			}
		})
	}
}
