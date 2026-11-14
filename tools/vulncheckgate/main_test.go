package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

const config = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scan_level":"symbol","scan_mode":"source"}}
{"SBOM":{"go_version":"go1.27.4","modules":[{"path":"github.com/elythia-network/elythia"},{"path":"github.com/labstack/echo/v4","version":"v4.16.3"}]}}
`

// osvs are the OSV messages govulncheck writes before the findings.
const osvs = `{"osv":{"id":"GO-2026-6872","summary":"Rate Limiter Bypass"}}
{"osv":{"id":"GO-2026-6875","summary":"Reverse Proxy Forwards X-Real-IP"}}
{"osv":{"id":"GO-2026-5932","summary":"openpgp is unmaintained"}}
`

// affected6872 reaches a function of echo; module6872 only the module.
const (
	module6872   = `{"finding":{"osv":"GO-2026-6872","trace":[{"module":"github.com/labstack/echo/v4","version":"v4.16.3"}]}}` + "\n"
	affected6872 = `{"finding":{"osv":"GO-2026-6872","trace":[{"module":"github.com/labstack/echo/v4","version":"v4.16.3","package":"github.com/labstack/echo/v4","function":"JSON","receiver":"*context"},{"module":"github.com/elythia-network/elythia","package":"github.com/elythia-network/elythia/internal/api/meta","function":"Ping","receiver":"*Handler"}]}}` + "\n"
	affected6875 = `{"finding":{"osv":"GO-2026-6875","trace":[{"module":"github.com/labstack/echo/v4","package":"github.com/labstack/echo/v4","function":"New"},{"module":"github.com/elythia-network/elythia","package":"github.com/elythia-network/elythia/internal/server","function":"New"}]}}` + "\n"
	// cryptoModule is required but not called: it is listed, not a failure.
	cryptoModule = `{"finding":{"osv":"GO-2026-5932","fixed_version":"v0.58.1","trace":[{"module":"golang.org/x/crypto","version":"v0.58.0"}]}}` + "\n"
	cryptoPkg    = `{"finding":{"osv":"GO-2026-5932","trace":[{"module":"golang.org/x/crypto","package":"golang.org/x/crypto/openpgp"}]}}` + "\n"
)

const allowBoth = "# comment\n\nGO-2026-6872\tgithub.com/labstack/echo/v4\tnot used\nGO-2026-6875\tgithub.com/labstack/echo/v4\tnot used\n"

func runGate(t *testing.T, allow, input string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run([]string{"-allow", write(t, "allow.tsv", allow), write(t, "vuln.json", input)}, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestGatePassesWhenEveryAffectedVulnIsAllowed(t *testing.T) {
	code, out, errOut := runGate(t, allowBoth, config+osvs+module6872+cryptoModule+cryptoPkg+affected6872+affected6872+affected6875)
	assert.Equal(t, 0, code, errOut)
	assert.Contains(t, out, "allowed: GO-2026-6872 (github.com/labstack/echo/v4): not used")
	assert.Contains(t, out, "allowed: GO-2026-6875")
	assert.Contains(t, out, "(2 allowed)")
	// require や import しているだけのものは、落とさず、許可の行にも出さない。
	assert.NotContains(t, out+errOut, "GO-2026-5932")
}

func TestGateFailsOnAnAffectedVulnOutsideTheAllowlist(t *testing.T) {
	allowOne := "GO-2026-6875\tgithub.com/labstack/echo/v4\tnot used\n"
	code, _, errOut := runGate(t, allowOne, config+osvs+affected6872+affected6875)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "affected: GO-2026-6872 (github.com/labstack/echo/v4): Rate Limiter Bypass")
	assert.Contains(t, errOut, "found in github.com/labstack/echo/v4@v4.16.3, fixed in (no fixed version)")
	assert.Contains(t, errOut, "called via github.com/elythia-network/elythia/internal/api/meta.Handler.Ping -> github.com/labstack/echo/v4.context.JSON")
	assert.NotContains(t, errOut, "GO-2026-6875")
}

func TestGateFailsOnACalledVulnThatWasOnlyRequiredBefore(t *testing.T) {
	// GO-2026-5932 が関数まで届くようになったら、許可の一覧に無いので落とす。
	called := `{"finding":{"osv":"GO-2026-5932","fixed_version":"v0.58.1","trace":[{"module":"golang.org/x/crypto","version":"v0.58.0","package":"golang.org/x/crypto/openpgp","function":"ReadEntity"},{"module":"github.com/elythia-network/elythia","package":"github.com/elythia-network/elythia/internal/x","function":"F"}]}}` + "\n"
	code, _, errOut := runGate(t, allowBoth, config+osvs+affected6872+affected6875+called)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "affected: GO-2026-5932 (golang.org/x/crypto)")
	assert.Contains(t, errOut, "found in golang.org/x/crypto@v0.58.0, fixed in v0.58.1")
}

// TestGateChecksEveryModuleOfAnAdvisory covers an advisory that names two
// modules (echo/v4 and echo/v5 for GO-2026-6872): allowing it for one module
// does not let a call into the other through, whichever finding comes first.
func TestGateChecksEveryModuleOfAnAdvisory(t *testing.T) {
	v5 := `{"finding":{"osv":"GO-2026-6872","fixed_version":"v5.1.0","trace":[{"module":"github.com/labstack/echo/v5","version":"v5.0.0","package":"github.com/labstack/echo/v5","function":"RealIP","receiver":"*Context"},{"module":"github.com/elythia-network/elythia","package":"github.com/elythia-network/elythia/internal/x","function":"F"}]}}` + "\n"
	for name, input := range map[string]string{
		"v4 first": config + osvs + affected6872 + v5 + affected6875,
		"v5 first": config + osvs + v5 + affected6872 + affected6875,
	} {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runGate(t, allowBoth, input)
			assert.Equal(t, 1, code)
			assert.Contains(t, errOut, "GO-2026-6872 is allowed for github.com/labstack/echo/v4, but the code calls it in github.com/labstack/echo/v5@v5.0.0 (fixed in v5.1.0)")
			assert.NotContains(t, out, "allowed: GO-2026-6872")
			assert.NotContains(t, errOut, "unused: GO-2026-6872")
		})
	}
}

// TestGateShowsTheFirstCallPath pins that the example call path of a failure
// is the first one govulncheck reported for the module.
func TestGateShowsTheFirstCallPath(t *testing.T) {
	other := strings.Replace(affected6872, `"function":"Ping"`, `"function":"Other"`, 1)
	code, _, errOut := runGate(t, "", config+osvs+affected6872+other)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "internal/api/meta.Handler.Ping -> ")
	assert.NotContains(t, errOut, "Handler.Other")
}

func TestGateFailsOnAnUnusedEntry(t *testing.T) {
	code, _, errOut := runGate(t, allowBoth, config+osvs+module6872+affected6875)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "unused: GO-2026-6872 is in the allowlist")
	assert.NotContains(t, errOut, "unused: GO-2026-6875")
}

func TestGateFailsWhenTheModuleDiffers(t *testing.T) {
	allow := "GO-2026-6872\tgithub.com/labstack/echo/v5\tnot used\nGO-2026-6875\tgithub.com/labstack/echo/v4\tnot used\n"
	code, out, errOut := runGate(t, allow, config+osvs+affected6872+affected6875)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "GO-2026-6872 is allowed for github.com/labstack/echo/v5, but the code calls it in github.com/labstack/echo/v4")
	// 許可した module (v5) では呼んでいないので、その行は使われていない。
	assert.Contains(t, errOut, "unused: GO-2026-6872 is in the allowlist for github.com/labstack/echo/v5")
	assert.NotContains(t, out, "allowed: GO-2026-6872")
}

func TestGateRejectsInputItCannotTrust(t *testing.T) {
	for _, tt := range []struct{ name, input, want string }{
		{"empty", "", "no govulncheck config message"},
		{"text output", "Scanning your code...\n", "reading govulncheck output"},
		{"no config", osvs + affected6872, "no govulncheck config message"},
		{"no SBOM", strings.SplitAfter(config, "\n")[0] + osvs + affected6872, "no SBOM message"},
		{"package level", strings.Replace(config, `"symbol"`, `"package"`, 1) + osvs, `scanned at the "package" level`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := runGate(t, allowBoth, tt.input)
			assert.Equal(t, 2, code)
			assert.Contains(t, errOut, tt.want)
		})
	}
}

func TestReadAllowlistRejectsBadRows(t *testing.T) {
	for _, tt := range []struct{ name, allow, want string }{
		{"two columns", "GO-2026-6872\tgithub.com/labstack/echo/v4\n", "want 3 tab-separated columns"},
		{"bad id", "CVE-2026-1\tgithub.com/labstack/echo/v4\tx\n", "is not a Go vulnerability id"},
		{"no module", "GO-2026-6872\t\tx\n", "has no module"},
		{"no reason", "GO-2026-6872\tgithub.com/labstack/echo/v4\t  \n", "has no reason"},
		{"duplicate", allowBoth + "GO-2026-6872\tgithub.com/labstack/echo/v4\tagain\n", "is listed twice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := runGate(t, tt.allow, config)
			assert.Equal(t, 2, code)
			assert.Contains(t, errOut, tt.want)
		})
	}
}

func TestRunUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	assert.Equal(t, 2, run(nil, &out, &errOut))
	assert.Contains(t, errOut.String(), "give one govulncheck JSON file")
	assert.Equal(t, 2, run([]string{"-nope"}, &out, &errOut))
	assert.Equal(t, 2, run([]string{"-allow", filepath.Join(t.TempDir(), "missing.tsv"), "x.json"}, &out, &errOut))
	assert.Equal(t, 2, run([]string{"-allow", write(t, "allow.tsv", allowBoth), filepath.Join(t.TempDir(), "missing.json")}, &out, &errOut))
}

// TestAllowlistIsReadable keeps the checked-in allowlist parseable. The echo
// rows go when the vulnerability database records v4.16.3 as the fix.
func TestAllowlistIsReadable(t *testing.T) {
	allow, err := readAllowlist("allow.tsv")
	require.NoError(t, err)
	for id, e := range allow {
		assert.NotEmpty(t, e.reason, id)
	}
	assert.Contains(t, allow, "GO-2026-6872")
	assert.Contains(t, allow, "GO-2026-6875")
}
