package hashtags_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashtags/show と hashtags/users の tag は、本家の paramDef と同じく 128 文字
// (code point) までで、超えたら INVALID_PARAM。数えるのは正規化 (NFKC) の前。
func TestShowAndUsers_TagLengthLimit(t *testing.T) {
	bodyOf := func(endpoint, tag string) string {
		m := map[string]any{"tag": tag}
		if endpoint == "users" {
			m["sort"] = "+follower"
		}
		b, err := json.Marshal(m)
		require.NoError(t, err)
		return string(b)
	}
	cases := []struct {
		name    string
		tag     string
		invalid bool
	}{
		{"at limit", strings.Repeat("あ", 128), false},
		{"over limit", strings.Repeat("あ", 129), true},
		// "e" + U+0301 は NFKC で 1 文字になる。65 組 = 正規化前 130 文字
		{"over limit before normalization", strings.Repeat("e\u0301", 65), true},
	}
	for _, endpoint := range []string{"show", "users"} {
		for _, tc := range cases {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				h := newHandler()
				handler := h.Show
				if endpoint == "users" {
					handler = h.Users
				}
				rec := doPost(handler, bodyOf(endpoint, tc.tag))
				var resp map[string]map[string]any
				if tc.invalid {
					require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					assert.Equal(t, "INVALID_PARAM", resp["error"]["code"])
					assert.Equal(t, "3d81ceae-475f-4600-b2a8-2bc116157532", resp["error"]["id"])
					return
				}
				// 上限以内は通常の処理へ進む (show は該当なしの NO_SUCH_HASHTAG、users は空の一覧)。
				if endpoint == "show" {
					require.Equal(t, http.StatusBadRequest, rec.Code)
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					assert.Equal(t, "NO_SUCH_HASHTAG", resp["error"]["code"])
				} else {
					assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				}
			})
		}
	}
}
