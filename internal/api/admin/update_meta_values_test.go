package admin_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/metasecret"
)

// The remote note cleaning settings are numbers in upstream's paramDef, but
// integer columns. A value that is not an integer is a 400 instead of
// upstream's 500 from the column (#3888); an integer written as 10.0 passes.
func TestUpdateMeta_RemoteNotesCleaningTakesIntegersOnly(t *testing.T) {
	for _, key := range []string{"remoteNotesCleaningExpiryDaysForEachNotes", "remoteNotesCleaningMaxProcessingDurationInMinutes"} {
		for body, ok := range map[string]bool{
			`{"` + key + `":1.5}`:  false,
			`{"` + key + `":"5"}`:  false,
			`{"` + key + `":null}`: false,
			`{"` + key + `":10.0}`: true,
			`{"` + key + `":-3}`:   true,
			`{"` + key + `":1e1}`:  true,
			`{"name":"x"}`:         true,
		} {
			t.Run(body, func(t *testing.T) {
				h, _, metaRepo, _ := newTestHandler(t)
				rec := doPost(h.UpdateMeta, body, nil)
				if ok {
					assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
					return
				}
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
				assert.Contains(t, rec.Body.String(), key+" must be an integer")
				assert.Nil(t, metaRepo.LastUpdateFields)
			})
		}
	}
}

// The relay settings get the lower bounds of the bundled frontend's inputs
// (pages/admin/relays.vue) and upper bounds their consumers do not overflow
// on, and the chunked upload limits the range of their int4 columns, which
// elythia_overlays.json declares too (#3888). The relay settings used to be
// saved as sent, 0 and -1 included.
func TestUpdateMeta_RelayAndChunkedUploadRanges(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"relayOrphanUserGraceDays":7}`:                  true,
		`{"relayOrphanUserGraceDays":6}`:                  false,
		`{"relayOrphanUserGraceDays":-1}`:                 false,
		`{"relayOrphanUserGraceDays":7.5}`:                false,
		`{"relayOrphanUserGraceDays":null}`:               false,
		`{"ephemeralRelayNoteTtlMinutes":1}`:              true,
		`{"ephemeralRelayNoteTtlMinutes":0}`:              false,
		`{"ephemeralRelayNoteTtlMinutes":"5"}`:            false,
		`{"relayOrphanUserGraceDays":3650}`:               true,
		`{"relayOrphanUserGraceDays":3651}`:               false,
		`{"ephemeralRelayNoteTtlMinutes":525600}`:         true,
		`{"ephemeralRelayNoteTtlMinutes":525601}`:         false,
		`{"chunkedUploadMaxSessionsPerUser":2147483647}`:  true,
		`{"chunkedUploadMaxSessionsPerUser":2147483648}`:  false,
		`{"chunkedUploadMaxPendingMbPerUser":2147483647}`: true,
		`{"chunkedUploadMaxPendingMbPerUser":2147483648}`: false,
		`{"chunkedUploadMaxPendingMbPerUser":0}`:          false,
	} {
		t.Run(body, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			rec := doPost(h.UpdateMeta, body, nil)
			if ok {
				assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				return
			}
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
			assert.Nil(t, metaRepo.LastUpdateFields)
		})
	}
}

// urlPreviewUserAgent and summalyProxy / urlPreviewSummaryProxyUrl are
// trimmed like JavaScript's trim, as upstream does (#3888): U+FEFF is
// whitespace there, U+0085 is not.
func TestUpdateMeta_TrimsLikeJavaScript(t *testing.T) {
	for _, tc := range []struct {
		name, body, column string
		want               any
	}{
		{"user agent of a BOM", `{"urlPreviewUserAgent":"\ufeff \u00a0"}`, "urlPreviewUserAgent", nil},
		{"user agent of a NEL", `{"urlPreviewUserAgent":"\u0085"}`, "urlPreviewUserAgent", "\u0085"},
		{"user agent kept untrimmed", `{"urlPreviewUserAgent":" UA\ufeff"}`, "urlPreviewUserAgent", " UA\ufeff"},
		{"proxy of a BOM", `{"urlPreviewSummaryProxyUrl":"\ufeff"}`, "urlPreviewSummaryProxyUrl", nil},
		{"proxy trimmed", `{"urlPreviewSummaryProxyUrl":"\ufeff https://p.example \ufeff"}`, "urlPreviewSummaryProxyUrl", "https://p.example"},
		{"proxy of a NEL", `{"summalyProxy":"\u0085"}`, "urlPreviewSummaryProxyUrl", "\u0085"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			rec := doPost(h.UpdateMeta, tc.body, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			require.Contains(t, metaRepo.LastUpdateFields, tc.column)
			assert.Equal(t, tc.want, metaRepo.LastUpdateFields[tc.column])
		})
	}
}

// The sensitive media detection URL and key become null only when they are
// "", like upstream's `ps.X === ” ? null : ps.X` (#3888). Whitespace is
// kept as sent.
func TestUpdateMeta_SensitiveMediaDetectionEmptyIsNull(t *testing.T) {
	for _, key := range []string{"sensitiveMediaDetectionApiUrl", "sensitiveMediaDetectionApiKey"} {
		for body, want := range map[string]any{
			`{"` + key + `":""}`:                    nil,
			`{"` + key + `":" "}`:                   " ",
			`{"` + key + `":" https://d.example "}`: " https://d.example ",
			`{"` + key + `":null}`:                  nil,
		} {
			t.Run(body, func(t *testing.T) {
				h, _, metaRepo, _ := newTestHandler(t)
				rec := doPost(h.UpdateMeta, body, nil)
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				require.Contains(t, metaRepo.LastUpdateFields, key)
				assert.Equal(t, want, metaRepo.LastUpdateFields[key])
			})
		}
	}
}

// A failed write of meta is a 500 with the endpoint and the fields in the
// log (#3888).
func TestUpdateMeta_WriteFailureIsLogged(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	logged := captureSlog(t)
	rec := doPost(h.UpdateMeta, `{"pinnedUsers":[1]}`, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	out := logged.String()
	assert.Contains(t, out, "level=ERROR")
	assert.Contains(t, out, "admin: writing meta failed")
	assert.Contains(t, out, "pinnedUsers")
	assert.Contains(t, out, "non-string element")
}

// The secret fields take a string or null only, also from a body the
// validator skips (#3888): the driver quotes a value it cannot write in its
// error, and GORM's logger writes that error as is.
func TestUpdateMeta_SecretFieldsTakeStringsOrNull(t *testing.T) {
	require.NotEmpty(t, metasecret.Fields)
	for _, f := range metasecret.Fields {
		for _, v := range []string{`{"k":"secret-in-object"}`, `["secret-in-array"]`, `1`, `true`} {
			body := `{"` + f + `":` + v + `,"name":"x"}`
			t.Run(body, func(t *testing.T) {
				h, _, metaRepo, _ := newTestHandler(t)
				rec := doPost(h.UpdateMeta, body, nil)
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), f+" must be a string or null")
				assert.Nil(t, metaRepo.LastUpdateFields)
			})
		}
		for _, v := range []string{`"s3cret"`, `null`} {
			body := `{"` + f + `":` + v + `}`
			t.Run(body, func(t *testing.T) {
				h, _, metaRepo, _ := newTestHandler(t)
				rec := doPost(h.UpdateMeta, body, nil)
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				assert.Contains(t, metaRepo.LastUpdateFields, f)
			})
		}
	}
}

// The column names the aliases are renamed to (termsOfServiceUrl,
// mcaptchaSitekey) are not upstream's params: they are ignored like
// upstream does, and only a value sent by the alias is written (#3888).
// They used to reach the UPDATE unvalidated ({"termsOfServiceUrl":{}} was a
// 500).
func TestUpdateMeta_ColumnNamesOfAliasesAreIgnored(t *testing.T) {
	for _, column := range []string{"termsOfServiceUrl", "mcaptchaSitekey"} {
		h, _, metaRepo, _ := newTestHandler(t)
		rec := doPost(h.UpdateMeta, `{"`+column+`":{},"name":"x"}`, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.NotContains(t, metaRepo.LastUpdateFields, column)
	}
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"tosUrl":"https://a.example","termsOfServiceUrl":"https://b.example",`+
		`"mcaptchaSiteKey":"by-alias","mcaptchaSitekey":"by-column"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "https://a.example", metaRepo.LastUpdateFields["termsOfServiceUrl"])
	assert.Equal(t, "by-alias", metaRepo.LastUpdateFields["mcaptchaSitekey"])
}

// The label of a question is counted after trimming by the handler, but as
// sent by the declaration (elythia_overlays.json). The handler alone takes a
// label of 100 characters with a space around, and rejects a label of only
// whitespace that the declaration lets through (#3888).
func TestUpdateMeta_SignupFormLabelIsTrimmedByTheHandler(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"signupApplicationForm":[{"label":" ","type":"text"}]}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = doPost(h.UpdateMeta, `{"signupApplicationForm":[{"label":" `+strings.Repeat("あ", 100)+` ","type":"text"}]}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}
