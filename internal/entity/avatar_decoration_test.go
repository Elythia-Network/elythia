package entity

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

type stubDecoLookup struct {
	urls map[string]string
}

func (s *stubDecoLookup) LookupURL(id string) (string, bool) {
	u, ok := s.urls[id]
	return u, ok
}

func TestPackUserLite_AvatarDecorations_EnrichedWithURL(t *testing.T) {
	t.Cleanup(func() { SetAvatarDecorationLookup(nil) })
	SetAvatarDecorationLookup(&stubDecoLookup{urls: map[string]string{
		"dec1": "https://cdn.example/dec1.png",
	}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1","angle":0.5,"flipH":true,"offsetX":-0.25,"offsetY":0.25}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 1)
	d := out.AvatarDecorations[0]
	assert.Equal(t, "dec1", d.ID)
	assert.Equal(t, 0.5, d.Angle)
	assert.True(t, d.FlipH)
	assert.Equal(t, -0.25, d.OffsetX)
	assert.Equal(t, 0.25, d.OffsetY)
	assert.Equal(t, "https://cdn.example/dec1.png", d.URL)
}

func TestPackUserLite_AvatarDecorations_DropsUnknownIDs(t *testing.T) {
	t.Cleanup(func() { SetAvatarDecorationLookup(nil) })
	SetAvatarDecorationLookup(&stubDecoLookup{urls: map[string]string{"dec1": "u1"}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1"},{"id":"deleted"}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 1)
	assert.Equal(t, "dec1", out.AvatarDecorations[0].ID)
}

func TestPackUserLite_AvatarDecorations_NoLookup(t *testing.T) {
	t.Cleanup(func() { SetAvatarDecorationLookup(nil) })
	SetAvatarDecorationLookup(nil)

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1","angle":1}]`)),
	}
	out := PackUserLite(u)
	// lookup 未配線時は url 空のまま全件残す (旧挙動と同じくフロント側で
	// catalog 解決させる fallback)。
	require.Len(t, out.AvatarDecorations, 1)
	assert.Equal(t, "dec1", out.AvatarDecorations[0].ID)
	assert.Equal(t, 1.0, out.AvatarDecorations[0].Angle)
	assert.Empty(t, out.AvatarDecorations[0].URL)
}

func TestPackUserLite_AvatarDecorations_EmptyOrNull(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"nil", nil},
		{"empty", []byte(``)},
		{"emptyArray", []byte(`[]`)},
		{"malformed", []byte(`{not json}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON(tc.raw)}
			out := PackUserLite(u)
			// JSON null ではなく空配列で出すこと。
			b, err := json.Marshal(out.AvatarDecorations)
			require.NoError(t, err)
			assert.Equal(t, "[]", string(b))
		})
	}
}

// #1781: angle / flipH / offsetX / offsetY は falsy (0 / false) のとき
// JSON から省く (upstream `ud.angle || undefined`)。非 falsy 値は残す。
func TestPackUserLite_AvatarDecorations_OmitsFalsyFields(t *testing.T) {
	SetAvatarDecorationLookup(&stubDecoLookup{urls: map[string]string{"dec1": "https://cdn.test/d1.png"}})
	defer SetAvatarDecorationLookup(nil)

	t.Run("all falsy fields omitted", func(t *testing.T) {
		u := &model.User{
			ID:                "u1",
			Username:          "alice",
			AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`)),
		}
		out := PackUserLite(u)
		b, err := json.Marshal(out.AvatarDecorations)
		require.NoError(t, err)
		s := string(b)
		assert.NotContains(t, s, "angle")
		assert.NotContains(t, s, "flipH")
		assert.NotContains(t, s, "offsetX")
		assert.NotContains(t, s, "offsetY")
		// id / url は常に残る。
		assert.Contains(t, s, "\"id\":\"dec1\"")
		assert.Contains(t, s, "\"url\":\"https://cdn.test/d1.png\"")
	})

	t.Run("non-falsy fields retained", func(t *testing.T) {
		u := &model.User{
			ID:                "u1",
			Username:          "alice",
			AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1","angle":0.5,"flipH":true,"offsetX":-0.25,"offsetY":0.25}]`)),
		}
		out := PackUserLite(u)
		b, err := json.Marshal(out.AvatarDecorations)
		require.NoError(t, err)
		s := string(b)
		assert.Contains(t, s, "\"angle\":0.5")
		assert.Contains(t, s, "\"flipH\":true")
		assert.Contains(t, s, "\"offsetX\":-0.25")
		assert.Contains(t, s, "\"offsetY\":0.25")
	})
}

func TestPackUserLite_AvatarDecorations_DropsEmptyIDEntry(t *testing.T) {
	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":""},{"angle":1}]`)),
	}
	out := PackUserLite(u)
	assert.Empty(t, out.AvatarDecorations)
}

// --- 絵文字由来のデコレーション (#2975) ---

type stubEmojiDecoLookup struct {
	entries map[string][2]string // id -> {url, name}
	calls   int
}

func (s *stubEmojiDecoLookup) LookupEmojiDecoration(id string) (string, string, bool) {
	s.calls++
	e, ok := s.entries[id]
	return e[0], e[1], ok
}

func TestPackUserLite_EmojiDecoration_Resolved(t *testing.T) {
	t.Cleanup(func() { SetEmojiDecorationLookup(nil) })
	SetEmojiDecorationLookup(&stubEmojiDecoLookup{entries: map[string][2]string{
		"e1": {"https://cdn.example/e1.png", "party"},
	}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"e1","emojiName":"party","angle":0.25}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 1)
	d := out.AvatarDecorations[0]
	assert.Equal(t, "e1", d.ID)
	assert.Equal(t, "https://cdn.example/e1.png", d.URL)
	assert.Equal(t, "party", d.EmojiName)
	assert.Equal(t, 0.25, d.Angle)
}

// **保存済みの名前ではなく現在の名前を出す。** `admin/emoji/update` は名前を
// 変えられるので、jsonb の写しを出すと古い名前が残り続ける。
func TestPackUserLite_EmojiDecoration_UsesCurrentName(t *testing.T) {
	t.Cleanup(func() { SetEmojiDecorationLookup(nil) })
	SetEmojiDecorationLookup(&stubEmojiDecoLookup{entries: map[string][2]string{
		"e1": {"https://cdn.example/e1.png", "renamed"},
	}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"e1","emojiName":"oldname"}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 1)
	assert.Equal(t, "renamed", out.AvatarDecorations[0].EmojiName)
}

// 削除された / センシティブになった絵文字は lookup が ok=false を返すので落ちる。
func TestPackUserLite_EmojiDecoration_DropsUnresolvable(t *testing.T) {
	t.Cleanup(func() {
		SetEmojiDecorationLookup(nil)
		SetAvatarDecorationLookup(nil)
	})
	SetEmojiDecorationLookup(&stubEmojiDecoLookup{entries: map[string][2]string{"e1": {"u", "ok"}}})
	SetAvatarDecorationLookup(&stubDecoLookup{urls: map[string]string{"dec1": "https://cdn.example/dec1.png"}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1"},{"id":"gone","emojiName":"gone"},{"id":"e1","emojiName":"ok"}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 2)
	assert.Equal(t, "dec1", out.AvatarDecorations[0].ID)
	assert.Equal(t, "e1", out.AvatarDecorations[1].ID)
}

// **未配線なら落とす (fail-closed)。** catalog 由来は url 抜きで残すが、絵文字は
// 「まだセンシティブでないか」を判断できないので出さない。
func TestPackUserLite_EmojiDecoration_DroppedWhenLookupUnwired(t *testing.T) {
	t.Cleanup(func() { SetEmojiDecorationLookup(nil) })
	SetEmojiDecorationLookup(nil)

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"e1","emojiName":"party"}]`)),
	}
	out := PackUserLite(u)
	assert.Empty(t, out.AvatarDecorations)
}

// catalog 由来のエントリを絵文字の lookup へ流さない (判別子は emojiName)。
func TestPackUserLite_CatalogDecoration_DoesNotUseEmojiLookup(t *testing.T) {
	t.Cleanup(func() {
		SetEmojiDecorationLookup(nil)
		SetAvatarDecorationLookup(nil)
	})
	emojiLookup := &stubEmojiDecoLookup{entries: map[string][2]string{}}
	SetEmojiDecorationLookup(emojiLookup)
	SetAvatarDecorationLookup(&stubDecoLookup{urls: map[string]string{"dec1": "https://cdn.example/dec1.png"}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1","emojiName":""}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 1)
	assert.Equal(t, "https://cdn.example/dec1.png", out.AvatarDecorations[0].URL)
	assert.Zero(t, emojiLookup.calls, "emojiName が空なら絵文字の lookup は引かない")
}

// emojiName は絵文字由来のときだけ出す (upstream には無い additive field)。
func TestAvatarDecorationItem_EmojiNameOmittedForCatalog(t *testing.T) {
	raw, err := json.Marshal(AvatarDecorationItem{ID: "dec1", URL: "https://cdn.example/dec1.png"})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "emojiName")

	raw, err = json.Marshal(AvatarDecorationItem{ID: "e1", URL: "u", EmojiName: "party"})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"emojiName":"party"`)
}

// --- サイズ (mk-go 独自の scale、#2975) ---

func TestPackUserLite_Decoration_ScaleRoundTrips(t *testing.T) {
	t.Cleanup(func() { SetAvatarDecorationLookup(nil) })
	SetAvatarDecorationLookup(&stubDecoLookup{urls: map[string]string{"dec1": "https://cdn.example/dec1.png"}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1","scale":0.4}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 1)
	require.NotNil(t, out.AvatarDecorations[0].Scale)
	assert.Equal(t, 0.4, *out.AvatarDecorations[0].Scale)
}

// **既定 (無い / 1) では出さない。** 既存の行は scale を持たないので、出すと
// 「既定のままの要素」の shape が upstream からずれる。
func TestPackUserLite_Decoration_ScaleOmittedWhenDefault(t *testing.T) {
	t.Cleanup(func() { SetAvatarDecorationLookup(nil) })
	SetAvatarDecorationLookup(&stubDecoLookup{urls: map[string]string{"dec1": "u", "dec2": "u"}})

	u := &model.User{
		ID:                "u1",
		Username:          "alice",
		AvatarDecorations: datatypes.JSON([]byte(`[{"id":"dec1"},{"id":"dec2","scale":1}]`)),
	}
	out := PackUserLite(u)
	require.Len(t, out.AvatarDecorations, 2)
	assert.Nil(t, out.AvatarDecorations[0].Scale)
	assert.Nil(t, out.AvatarDecorations[1].Scale)

	raw, err := json.Marshal(out.AvatarDecorations)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "scale")
}
