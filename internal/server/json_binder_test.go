package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/optional"
)

// deserializeAt runs fastJSONSerializer.Deserialize on body for a request
// routed to path (the registered route pattern, as c.Path() reports it).
func deserializeAt(t *testing.T, path, body string, dst any) (echo.Context, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(req, httptest.NewRecorder())
	c.SetPath(path)
	return c, fastJSONSerializer{}.Deserialize(c, dst)
}

type exactInner struct {
	Value int `json:"value"`
}

type exactBase struct {
	BaseField string `json:"baseField"`
	Shadowed  string `json:"name"`
}

type exactTarget struct {
	exactBase
	ReportID *string                   `json:"reportId"`
	Name     string                    `json:"name"`
	Untagged string                    // v1 は Go の field 名 "Untagged" で照合する
	Inner    exactInner                `json:"inner"`
	InnerPtr *exactInner               `json:"innerPtr"`
	Items    []exactInner              `json:"items"`
	Fixed    [1]exactInner             `json:"fixed"`
	ByKey    map[string]exactInner     `json:"byKey"`
	Raw      json.RawMessage           `json:"raw"`
	Any      any                       `json:"any"`
	Opt      optional.Nullable[string] `json:"opt"`
	Skipped  string                    `json:"-"`
	Key      string                    `json:"key"`
}

// TestDeserialize_ExactKeys pins that object keys bind to struct fields only
// on an exact match (#3330). encoding/json alone also accepts a
// case-insensitive match, so every row's want differs from plain v1 decoding
// except where noted.
func TestDeserialize_ExactKeys(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, got exactTarget)
	}{
		{"case variant alone is absent", `{"reportid":"r1"}`, func(t *testing.T, got exactTarget) {
			assert.Nil(t, got.ReportID)
		}},
		{"later case variant does not override", `{"reportId":"r1","REPORTID":null}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, str("r1"), got.ReportID)
		}},
		{"untagged field matches its Go name only", `{"Untagged":"x","untagged":"y"}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, "x", got.Untagged)
		}},
		{"nested struct", `{"inner":{"value":2,"VALUE":3}}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, 2, got.Inner.Value)
		}},
		{"case variant of a nested struct field", `{"INNER":{"value":2}}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, 0, got.Inner.Value)
		}},
		{"pointer to struct", `{"innerPtr":{"Value":5}}`, func(t *testing.T, got exactTarget) {
			require.NotNil(t, got.InnerPtr)
			assert.Equal(t, 0, got.InnerPtr.Value)
		}},
		{"slice of struct", `{"items":[{"Value":1},{"value":2}]}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, []exactInner{{0}, {2}}, got.Items)
		}},
		{"array of struct", `{"fixed":[{"VALUE":1}]}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, 0, got.Fixed[0].Value)
		}},
		{"map of struct keeps map keys", `{"byKey":{"K":{"Value":1,"value":2},"k":{"value":3}}}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, map[string]exactInner{"K": {2}, "k": {3}}, got.ByKey)
		}},
		{"embedded promoted field", `{"baseField":"y","BASEFIELD":"z"}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, "y", got.BaseField)
		}},
		{"outer field shadows embedded one", `{"name":"n","NAME":"m"}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, "n", got.Name)
			assert.Empty(t, got.Shadowed)
		}},
		{"raw message is kept verbatim", `{"raw":{"A":1,"a":2}}`, func(t *testing.T, got exactTarget) {
			assert.JSONEq(t, `{"A":1,"a":2}`, string(got.Raw))
		}},
		{"interface value is kept verbatim", `{"any":{"X":1}}`, func(t *testing.T, got exactTarget) {
			assert.Equal(t, map[string]any{"X": float64(1)}, got.Any)
		}},
		{"custom unmarshaler gets the exact key only", `{"opt":"a","OPT":null}`, func(t *testing.T, got exactTarget) {
			require.True(t, got.Opt.Present)
			assert.Equal(t, str("a"), got.Opt.Value)
		}},
		{"json:\"-\" field never binds", `{"Skipped":"x"}`, func(t *testing.T, got exactTarget) {
			assert.Empty(t, got.Skipped)
		}},
		{"unicode case folding does not match", "{\"Key\":\"kelvin\"}", func(t *testing.T, got exactTarget) {
			assert.Empty(t, got.Key)
		}},
		{"escaped exact key matches", "{\"k" + `\` + "u0065y\":\"x\"}", func(t *testing.T, got exactTarget) {
			assert.Equal(t, "x", got.Key)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got exactTarget
			_, err := deserializeAt(t, "/api/test", tc.body, &got)
			require.NoError(t, err)
			tc.check(t, got)
		})
	}
}

// TestDeserialize_MapTarget pins that a map destination keeps every key: a
// map has no fields to fold onto.
func TestDeserialize_MapTarget(t *testing.T) {
	var got map[string]any
	_, err := deserializeAt(t, "/api/admin/update-meta", `{"a":1,"A":2}`, &got)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": float64(1), "A": float64(2)}, got)
}

// TestDeserialize_ErrorsUnchanged pins that bodies the filter cannot walk
// still produce encoding/json's own errors.
func TestDeserialize_ErrorsUnchanged(t *testing.T) {
	var got exactTarget
	_, err := deserializeAt(t, "/x", `{"name":`, &got)
	require.Error(t, err)

	_, err = deserializeAt(t, "/x", `{"inner":{"value":"str"}}`, &got)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	assert.Contains(t, he.Message, "Unmarshal type error")

	_, err = deserializeAt(t, "/x", `{"items":{"value":1}}`, &got)
	require.ErrorAs(t, err, &he)
	assert.Contains(t, he.Message, "Unmarshal type error")
}

// TestDeserialize_NonObjectBody pins upstream's ajv `type: 'object'` failure
// for an API endpoint body that is valid JSON but not an object.
func TestDeserialize_NonObjectBody(t *testing.T) {
	for _, body := range []string{`null`, ` null`, `[]`, `[{"a":1}]`, `"x"`, `1`, `-1`, `true`, `false`} {
		t.Run(body, func(t *testing.T) {
			var got exactTarget
			c, err := deserializeAt(t, "/api/notes/create", body, &got)
			assert.Same(t, errBodyNotObject, err)
			assert.Equal(t, true, c.Get(apierr.BodyNotObjectContextKey))
		})
	}
	t.Run("map target", func(t *testing.T) {
		var got map[string]any
		_, err := deserializeAt(t, "/api/admin/update-meta", `null`, &got)
		assert.Same(t, errBodyNotObject, err)
	})
}

// TestDeserialize_NonObjectBodyOutOfScope pins where the non-object check
// does not apply: non-API routes, upstream's non-endpoint /api routes,
// destinations that do not read an object, and malformed bodies.
func TestDeserialize_NonObjectBodyOutOfScope(t *testing.T) {
	var s exactTarget
	c, err := deserializeAt(t, "/oauth/token", `null`, &s)
	require.NoError(t, err)
	assert.Nil(t, c.Get(apierr.BodyNotObjectContextKey))

	_, err = deserializeAt(t, "/api/signup", `null`, &s)
	require.NoError(t, err)

	_, err = deserializeAt(t, "/api/miauth/:session/check", `null`, &s)
	require.NoError(t, err)

	var list []exactInner
	_, err = deserializeAt(t, "/api/x", `[{"value":1,"VALUE":2}]`, &list)
	require.NoError(t, err)
	assert.Equal(t, []exactInner{{1}}, list)

	var opt optional.Nullable[string]
	_, err = deserializeAt(t, "/api/x", `"v"`, &opt)
	require.NoError(t, err)

	c, err = deserializeAt(t, "/api/x", `x`, &s)
	require.Error(t, err)
	assert.NotSame(t, errBodyNotObject, err)
	assert.Nil(t, c.Get(apierr.BodyNotObjectContextKey))

	c, err = deserializeAt(t, "/api/x", ``, &s)
	require.Error(t, err)
	assert.Nil(t, c.Get(apierr.BodyNotObjectContextKey))
}
