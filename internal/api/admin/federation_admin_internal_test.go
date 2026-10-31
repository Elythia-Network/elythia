package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFederationUpdateInstanceRequest_NotePointerSemantics は #675 の
// 「moderationNote を空文字列で clear できない」バグの regression guard。
// pointer 化により JSON decode 境界で「未送信 (nil)」「明示的 ""」「明示的
// "value"」を分離できることを直接検証する。
func TestFederationUpdateInstanceRequest_NotePointerSemantics(t *testing.T) {
	t.Run("note omitted leaves pointer nil", func(t *testing.T) {
		var req federationUpdateInstanceRequest
		require.NoError(t, json.Unmarshal([]byte(`{"host":"a"}`), &req))
		assert.Nil(t, req.ModerationNote, "omitted field must yield nil pointer")
		assert.NotContains(t, req.updates(), "moderationNote",
			"omitted field must not appear in updates payload")
	})

	t.Run("note explicit empty string clears", func(t *testing.T) {
		var req federationUpdateInstanceRequest
		require.NoError(t, json.Unmarshal([]byte(`{"host":"a","moderationNote":""}`), &req))
		require.NotNil(t, req.ModerationNote, "explicit \"\" must yield non-nil pointer")
		assert.Equal(t, "", *req.ModerationNote)
		// #675 の核心: 明示的 "" は updates に伝搬し、DB へ書き込まれる
		updates := req.updates()
		require.Contains(t, updates, "moderationNote")
		assert.Equal(t, "", updates["moderationNote"])
	})

	t.Run("note explicit value updates", func(t *testing.T) {
		var req federationUpdateInstanceRequest
		require.NoError(t, json.Unmarshal([]byte(`{"host":"a","moderationNote":"hello"}`), &req))
		require.NotNil(t, req.ModerationNote)
		assert.Equal(t, "hello", *req.ModerationNote)
		assert.Equal(t, "hello", req.updates()["moderationNote"])
	})
}

// TestFederationUpdateInstanceRequest_UpdatesIncludesAllSentFields は他の
// pointer fields が送信されたときだけ updates に含まれる契約を guard する。
// partial update 互換のため同じ pointer-vs-default 区別が必要。
//
// #715 / #724: isSuspended は SQL 列に存在しないので suspensionState enum に
// 変換する。
func TestFederationUpdateInstanceRequest_UpdatesIncludesAllSentFields(t *testing.T) {
	tt := true
	ff := false
	note := "n"
	cases := []struct {
		name string
		req  federationUpdateInstanceRequest
		want map[string]any
	}{
		{
			name: "all nil -> empty",
			req:  federationUpdateInstanceRequest{},
			want: map[string]any{},
		},
		{
			name: "isSuspended true → suspensionState manuallySuspended",
			req:  federationUpdateInstanceRequest{IsSuspended: &tt},
			want: map[string]any{"suspensionState": "manuallySuspended"},
		},
		{
			name: "isSuspended false → suspensionState none",
			req:  federationUpdateInstanceRequest{IsSuspended: &ff},
			want: map[string]any{"suspensionState": "none"},
		},
		{
			name: "moderationNote set",
			req:  federationUpdateInstanceRequest{ModerationNote: &note},
			want: map[string]any{"moderationNote": "n"},
		},
		{
			name: "all set together",
			req: federationUpdateInstanceRequest{
				IsSuspended:    &tt,
				ModerationNote: &note,
			},
			want: map[string]any{
				"suspensionState": "manuallySuspended",
				"moderationNote":  "n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.req.updates())
		})
	}
}
