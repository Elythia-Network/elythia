package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ajvCorpusFile is the corpus of upstream's ajv verdicts (#3673).
const ajvCorpusFile = "../api/paramschema/testdata/ajv_corpus.jsonl"

// ajvCase is one input of the corpus and ajv's verdict on it.
type ajvCase struct {
	ep   string
	line int
	in   json.RawMessage
	ok   bool
	path string
	msg  string
}

// readAJVCorpus returns the corpus cases of the endpoints in eps.
func readAJVCorpus(t *testing.T, eps []string) []ajvCase {
	t.Helper()
	b, err := os.ReadFile(ajvCorpusFile)
	require.NoError(t, err)
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var cases []ajvCase
	ep := ""
	for n := 1; sc.Scan(); n++ {
		if n == 1 {
			continue // header
		}
		var l struct {
			EP   string          `json:"ep"`
			In   json.RawMessage `json:"in"`
			OK   *bool           `json:"ok"`
			Path string          `json:"path"`
			Msg  string          `json:"msg"`
		}
		require.NoError(t, json.Unmarshal(sc.Bytes(), &l), "corpus line %d", n)
		if l.EP != "" {
			ep = l.EP
			continue
		}
		if !slices.Contains(eps, ep) {
			continue
		}
		require.NotNil(t, l.OK, "corpus line %d", n)
		cases = append(cases, ajvCase{ep: ep, line: n, in: l.In, ok: *l.OK, path: l.Path, msg: l.Msg})
	}
	require.NoError(t, sc.Err())
	return cases
}

// TestEnforcedEndpointsAreUpstreamEndpoints keeps paramEnforcedEndpoints to
// names upstream has, and to whole packages: an endpoint of a package left
// out would keep the handwritten checks only.
func TestEnforcedEndpointsAreUpstreamEndpoints(t *testing.T) {
	eps, err := paramschema.Upstream()
	require.NoError(t, err)
	packages := map[string]bool{}
	for _, name := range paramEnforcedEndpoints {
		ep, ok := eps[name]
		require.True(t, ok, "%s is not an upstream endpoint", name)
		require.NoError(t, ep.CompileErr, name)
		pkg, _, _ := strings.Cut(name, "/")
		packages[pkg] = true
	}
	require.NotEmpty(t, packages)
	for name := range eps {
		pkg, _, _ := strings.Cut(name, "/")
		if packages[pkg] {
			assert.Contains(t, paramEnforcedEndpoints, name, "package %s is enforced, but not %s", pkg, name)
		}
	}
}

// TestEnforcedEndpointsMatchTheCorpus sends every corpus input of the
// enforced endpoints to a real server and compares the answer with ajv's
// verdict: an input ajv rejects gets upstream's 400 INVALID_PARAM with the
// same info.param and info.reason, and an input ajv accepts gets no
// INVALID_PARAM (the handler may still answer NO_SUCH_CLIP and the like).
// The second half catches a handwritten check that is stricter than
// upstream, which the validator in front cannot fix.
func TestEnforcedEndpointsMatchTheCorpus(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB
	const userID, token = "penforce-user", "penforcetoken001"
	cleanup := func() {
		require.NoError(t, db.Exec(`DELETE FROM clip WHERE "userId" = ?`, userID).Error)
		require.NoError(t, db.Where("id = ?", userID).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)
	tok := token
	require.NoError(t, repository.NewUserRepository(db).Create(&model.User{
		ID: userID, Username: "penforce", UsernameLower: "penforce", Token: &tok,
	}))
	_, on, _ := newParamShadowServers(t)

	cases := readAJVCorpus(t, paramEnforcedEndpoints)
	seen := map[string][2]int{}
	for _, tc := range cases {
		n := seen[tc.ep]
		if tc.ok {
			n[0]++
		} else {
			n[1]++
		}
		seen[tc.ep] = n
	}
	for _, name := range paramEnforcedEndpoints {
		// 何も流さずに緑にならないよう、各endpointに受理と拒否の両方を要求する。
		require.Positive(t, seen[name][0], "the corpus has no accepted input for %s", name)
		require.Positive(t, seen[name][1], "the corpus has no rejected input for %s", name)
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/"+tc.ep, bytes.NewReader(tc.in))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		rec := httptest.NewRecorder()
		on.Handler().ServeHTTP(rec, req)

		var env struct {
			Error struct {
				Code string `json:"code"`
				Info struct {
					Param  string `json:"param"`
					Reason string `json:"reason"`
				} `json:"info"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		name := tc.ep + " " + string(tc.in)
		if tc.ok {
			assert.NotEqual(t, "INVALID_PARAM", env.Error.Code, "%s (corpus line %d): ajv accepts it; body %s", name, tc.line, rec.Body.String())
			continue
		}
		if assert.Equal(t, http.StatusBadRequest, rec.Code, "%s (corpus line %d)", name, tc.line) {
			assert.Equal(t, "INVALID_PARAM", env.Error.Code, name)
			assert.Equal(t, tc.path, env.Error.Info.Param, name)
			assert.Equal(t, tc.msg, env.Error.Info.Reason, name)
		}
	}
}
