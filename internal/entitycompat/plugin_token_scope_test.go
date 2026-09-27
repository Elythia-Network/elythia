package entitycompat

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// buildWithPluginsWorkflow is the reusable workflow that clones private plugins.
const buildWithPluginsWorkflow = ".github/workflows/build-with-plugins.yml"

// pluginTokenRe matches any mention of the private-plugin token.
//
// **大文字小文字を問わず、名前だけで拾う。** GitHub Actions の式は context の
// プロパティ名を case-insensitive に解決するので `secrets.PLUGIN_TOKEN` も
// `secrets.plugin_token` も同じ値になる。`secrets['plugin_token']` の添字記法も
// ある。`secrets.plugin_token` の形だけを見ると、別の step の `run:` に直書き
// した式が素通りする (実測)。step の中で名前が出るなら token を扱っているとみなす
// (fail-closed)。
var pluginTokenRe = regexp.MustCompile(`(?i)plugin_token`)

// TestPluginTokenIsScopedToCloneStep checks that the private-plugin token is
// written to the global git config only inside the step that clones, and is
// removed by an EXIT trap in that same step.
//
// **token を `~/.gitconfig` に残したまま後続へ進ませない。** 以前は token を
// 書く step と clone する step が分かれていて、書いたまま消していなかった。
// 後続の `go run ./tools/pluginbuild` や `pnpm install` (依存パッケージの
// lifecycle script が走る) からも読めたうえ、呼び出し元の token は他の
// private repository にも届く。
//
// 見るのは 3 点: (a) token に触れる step が 1 つだけで、workflow / job の env
// から全 step へ配っていない、(b) その step が clone (`tools/pluginresolve`) を
// 実行する、(c) その step の後始末が壊れていない (`tokenCleanupProblems`)。
// trap にしてあるのは、clone が失敗して `set -e` で抜けたときも消すため。
func TestPluginTokenIsScopedToCloneStep(t *testing.T) {
	wf := parseWorkflow(t, readRepoFile(t, buildWithPluginsWorkflow))

	require.Emptyf(t, wf.sharedEnvMentions(pluginTokenRe),
		"%s: plugin_token を workflow / job の env (または job の secrets) で配らないこと。全 step から読める", buildWithPluginsWorkflow)

	var tokenSteps []workflowStep
	for _, s := range wf.steps {
		if s.mentions(pluginTokenRe) {
			tokenSteps = append(tokenSteps, s)
		}
	}
	require.Equalf(t, 1, len(tokenSteps),
		"%s で plugin_token を扱う step は clone する 1 つだけにすること (見つかった step: %v)",
		buildWithPluginsWorkflow, stepNames(tokenSteps))

	s := tokenSteps[0]
	assert.Containsf(t, s.Run, "tools/pluginresolve",
		"step %q は token を設定するのに clone しない。設定と clone を同じ step に置くこと", s.Name)
	assert.Emptyf(t, tokenCleanupProblems(s.Run),
		"step %q の token の後始末が壊れている", s.Name)
}

// workflowStep is one step of a workflow job.
type workflowStep struct {
	Name string
	Run  string
	// scalars holds every scalar value of the step (run / env / with / if ...).
	scalars []string
}

// mentions reports whether any scalar of s matches re.
func (s workflowStep) mentions(re *regexp.Regexp) bool {
	for _, v := range s.scalars {
		if re.MatchString(v) {
			return true
		}
	}
	return false
}

// parsedWorkflow is the subset of a workflow file this gate reads.
type parsedWorkflow struct {
	steps []workflowStep
	// shared holds the scalars that reach every step of a job: the
	// workflow-level env, and each job's env / secrets.
	shared []string
}

// sharedEnvMentions returns the shared scalars that match re.
func (w parsedWorkflow) sharedEnvMentions(re *regexp.Regexp) []string {
	var out []string
	for _, v := range w.shared {
		if re.MatchString(v) {
			out = append(out, v)
		}
	}
	return out
}

// parseWorkflow reads a workflow file into steps and shared scalars.
func parseWorkflow(t *testing.T, body string) parsedWorkflow {
	t.Helper()
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(body), &root))
	require.NotEmpty(t, root.Content, "workflow が空")
	doc := root.Content[0]

	var wf parsedWorkflow
	if env := mappingValue(doc, "env"); env != nil {
		wf.shared = append(wf.shared, collectScalars(env)...)
	}
	jobs := mappingValue(doc, "jobs")
	require.NotNil(t, jobs, "workflow に jobs が無い")
	for i := 1; i < len(jobs.Content); i += 2 {
		job := jobs.Content[i]
		for _, key := range []string{"env", "secrets"} {
			if v := mappingValue(job, key); v != nil {
				wf.shared = append(wf.shared, collectScalars(v)...)
			}
		}
		steps := mappingValue(job, "steps")
		if steps == nil {
			continue
		}
		for _, st := range steps.Content {
			s := workflowStep{scalars: collectScalars(st)}
			if n := mappingValue(st, "name"); n != nil {
				s.Name = n.Value
			}
			if r := mappingValue(st, "run"); r != nil {
				s.Run = r.Value
			}
			wf.steps = append(wf.steps, s)
		}
	}
	require.NotEmpty(t, wf.steps, "workflow から step を 1 つも読めない")
	return wf
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// collectScalars returns every scalar (keys and values) under n.
func collectScalars(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, collectScalars(c)...)
	}
	return out
}

// stepNames lists step names for diagnostics.
func stepNames(steps []workflowStep) []string {
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name)
	}
	return names
}

var (
	// gitConfigSetRe captures the key of a `git config --global <key> <value>`.
	// `git config set --global` (git 2.46 以降の書き方) と `--add` / `--replace-all` も拾う。
	gitConfigSetRe = regexp.MustCompile(
		`git\s+config\s+(?:set\s+)?--global\s+(?:--(?:add|replace-all)\s+)?("[^"]*"|'[^']*'|[^\s-]\S*)`)
	// gitConfigUnsetRe captures the key removed by `git config --global --unset-all <key>`.
	gitConfigUnsetRe = regexp.MustCompile(
		`git\s+config\s+--global\s+--unset-all\s+("[^"]*"|'[^']*'|\S+)`)
	// trapWordRe finds the `trap` builtin anywhere in a line.
	trapWordRe = regexp.MustCompile(`(?:^|[\s;&|(])trap\s`)
)

// trapSignals splits a standalone `trap <action> <signal>...` line into its
// action and signals. ok is false when the line is not a standalone trap.
func trapSignals(line string) (action string, signals []string, ok bool) {
	rest, found := strings.CutPrefix(line, "trap ")
	if !found {
		return "", nil, false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", nil, false
	}
	if q := rest[0]; q == '\'' || q == '"' {
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return "", nil, false
		}
		action = rest[1 : end+1]
		rest = rest[end+2:]
	} else {
		f := strings.Fields(rest)
		action, rest = f[0], strings.Join(f[1:], " ")
	}
	return action, strings.Fields(rest), true
}

// coversExit reports whether signals include the shell's EXIT pseudo-signal.
func coversExit(signals []string) bool {
	for _, s := range signals {
		switch strings.ToUpper(s) {
		case "EXIT", "SIGEXIT", "0":
			return true
		}
	}
	return false
}

// unquote strips one layer of matching quotes.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// tokenCleanupProblems returns what is wrong with how script removes the token
// it writes to the global git config. An empty result means the cleanup holds.
//
// **trap が 1 つあるだけでは足りない。** bash の trap は同じシグナルに対して
// 後勝ちなので、後ろに `trap 'rm -rf ...' EXIT` を 1 行足すだけで unset が
// 消える (実測で旧版の gate は素通りした)。要求するのは次の全部:
//
//   - EXIT に掛かる trap がちょうど 1 つ (`trap - EXIT` での解除も 1 つと数える)
//   - その trap が `--unset-all` する
//   - trap が `git config --global` の書き込みより**前**にある (書いた後、trap を
//     張る前に落ちると消えない)
//   - 書き込むキーが全て、unset するキーと同じ
//   - `trap` を行頭以外 (`foo; trap ...`) に書かない。そこは検出できないので、
//     書き方で塞ぐ (fail-closed)
func tokenCleanupProblems(script string) []string {
	var problems []string
	exitTrapLine, firstSetLine := -1, -1
	exitTraps := 0
	var unsetKey string
	var setKeys []string

	for i, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if action, signals, ok := trapSignals(line); ok {
			if coversExit(signals) {
				exitTraps++
				exitTrapLine = i
				if m := gitConfigUnsetRe.FindStringSubmatch(action); m != nil {
					unsetKey = unquote(m[1])
				}
			}
			continue
		}
		if trapWordRe.MatchString(line) {
			problems = append(problems, fmt.Sprintf("行 %d: trap は行頭に単独で書くこと (%q)", i+1, line))
		}
		for _, m := range gitConfigSetRe.FindAllStringSubmatch(line, -1) {
			setKeys = append(setKeys, unquote(m[1]))
			if firstSetLine < 0 {
				firstSetLine = i
			}
		}
	}

	switch {
	case exitTraps == 0:
		problems = append(problems, "EXIT の trap が無い")
	case exitTraps > 1:
		problems = append(problems, fmt.Sprintf("EXIT の trap が %d 個ある。後勝ちなので unset が上書きされる", exitTraps))
	}
	if exitTraps == 1 && unsetKey == "" {
		problems = append(problems, "EXIT の trap が `git config --global --unset-all` していない")
	}
	if len(setKeys) == 0 {
		problems = append(problems, "`git config --global` で token を書く行が見つからない (書き方が変わったなら gate も直すこと)")
	}
	if exitTraps == 1 && firstSetLine >= 0 && exitTrapLine > firstSetLine {
		problems = append(problems, "EXIT の trap が `git config --global` の書き込みより後ろにある")
	}
	for _, k := range setKeys {
		if unsetKey != "" && k != unsetKey {
			problems = append(problems, fmt.Sprintf("書き込むキー %q と unset するキー %q が違う", k, unsetKey))
		}
	}
	return problems
}

// TestTokenCleanupProblems pins the cleanup checker with synthetic scripts.
func TestTokenCleanupProblems(t *testing.T) {
	const good = `set -eu
if [ -n "${PLUGIN_TOKEN:-}" ]; then
  key="url.https://x-access-token:${PLUGIN_TOKEN}@github.com/.insteadOf"
  trap 'rc=$?; git config --global --unset-all "$key" || rc=1; exit "$rc"' EXIT
  git config --global "$key" "https://github.com/"
fi
printf '%s\n' "$MK_PLUGINS" | go run ./tools/pluginresolve
`
	tests := []struct {
		name   string
		script string
		ok     bool
	}{
		{"current shape", good, true},
		{"second exit trap overrides unset", strings.Replace(good,
			"printf '%s", "trap 'rm -rf plugins/.tmp' EXIT\nprintf '%s", 1), false},
		{"trap reset", strings.Replace(good, "printf '%s", "trap - EXIT\nprintf '%s", 1), false},
		{"trap after set", strings.Replace(strings.Replace(good,
			"  trap 'rc=$?; git config --global --unset-all \"$key\" || rc=1; exit \"$rc\"' EXIT\n", "", 1),
			"https://github.com/\"\n", "https://github.com/\"\n  trap 'git config --global --unset-all \"$key\"' EXIT\n", 1), false},
		{"unset key differs", strings.Replace(good, `--unset-all "$key"`, `--unset-all "$other"`, 1), false},
		{"extra set with other key", strings.Replace(good,
			"https://github.com/\"\n", "https://github.com/\"\n  git config --global \"$key2\" x\n", 1), false},
		{"no trap", strings.Replace(good, "' EXIT", "' ERR", 1), false},
		{"trap without unset", strings.Replace(good, "git config --global --unset-all \"$key\" || rc=1; ", "", 1), false},
		{"inline trap", strings.Replace(good, "printf '%s", "true; trap 'rm -rf x' EXIT\nprintf '%s", 1), false},
		{"no set", strings.Replace(good, "  git config --global \"$key\" \"https://github.com/\"\n", "", 1), false},
		{"exit trap with other signals", strings.Replace(good, "' EXIT", "' EXIT INT TERM", 1), true},
		{"new-style set", strings.Replace(good, `git config --global "$key"`, `git config set --global "$key"`, 1), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenCleanupProblems(tt.script)
			if tt.ok {
				require.Empty(t, got)
			} else {
				require.NotEmpty(t, got)
			}
		})
	}
}

// TestPluginTokenStepDetection pins which workflow shapes count as a step
// handling the token.
func TestPluginTokenStepDetection(t *testing.T) {
	const wf = `
on: workflow_call
env:
  A: x
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: env
        env:
          T: ${{ secrets.plugin_token }}
        run: echo
      - name: run upper
        run: echo "${{ secrets.PLUGIN_TOKEN }}"
      - name: index
        run: echo "${{ secrets['plugin_token'] }}"
      - name: with
        uses: some/action@0123456789012345678901234567890123456789
        with:
          token: ${{ secrets.Plugin_Token }}
      - name: clean
        run: echo ok
`
	w := parseWorkflow(t, wf)
	var got []string
	for _, s := range w.steps {
		if s.mentions(pluginTokenRe) {
			got = append(got, s.Name)
		}
	}
	require.Equal(t, []string{"env", "run upper", "index", "with"}, got)
	require.Empty(t, w.sharedEnvMentions(pluginTokenRe))

	shared := parseWorkflow(t, `
env:
  T: ${{ secrets.plugin_token }}
jobs:
  a:
    secrets:
      plugin_token: ${{ secrets.plugin_token }}
    steps:
      - run: echo
`)
	require.Len(t, shared.sharedEnvMentions(pluginTokenRe), 3)
}
