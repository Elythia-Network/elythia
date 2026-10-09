package federation_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// utf16Len is the JavaScript string length of s.
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func remoteNoteJSON(t *testing.T, id string, fields map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           id,
		"type":         "Note",
		"attributedTo": "https://remote.example/users/alice",
		"to":           []string{"https://www.w3.org/ns/activitystreams#Public"},
	}
	for k, v := range fields {
		doc[k] = v
	}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return b
}

// リモートの本文は本家 DB_MAX_NOTE_TEXT_LENGTH (8192、JavaScript の length =
// UTF-16 の単位) で切る。3 つの取り出し方 (source / _misskey_content / content)
// のどれでも同じ。BMP 外の文字は 2 単位に数え、ペアの途中では切らない。
func TestIngestNote_ClipsRemoteTextToUpstreamLength(t *testing.T) {
	long := strings.Repeat("a", 9000)
	astral := strings.Repeat("\U0001F600", 5000) // 10000 UTF-16 units
	cases := []struct {
		name   string
		fields map[string]any
		want   int
	}{
		{"source", map[string]any{"source": map[string]any{"content": long, "mediaType": "text/x.misskeymarkdown"}, "content": "<p>x</p>"}, 8192},
		{"_misskey_content", map[string]any{"_misskey_content": long, "content": "<p>x</p>"}, 8192},
		{"content (HTML)", map[string]any{"content": "<p>" + long + "</p>"}, 8192},
		{"astral", map[string]any{"_misskey_content": astral}, 8192},
		// 8191 単位の後ろに BMP 外の文字が来る境界。入れると 8193 になるので
		// 手前で止めて 8191 にする。
		{"astral at odd boundary", map[string]any{"_misskey_content": "a" + astral}, 8191},
		{"short (control)", map[string]any{"_misskey_content": "hello"}, 5},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolver(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"), &stubFetcher{body: []byte(sampleActor)}, idGen)
			note, err := r.IngestNote(remoteNoteJSON(t, "https://remote.example/notes/len-"+string(rune('a'+i)), tc.fields))
			require.NoError(t, err)
			require.NotNil(t, note.Text)
			assert.Equal(t, tc.want, utf16Len(*note.Text))
			if strings.HasPrefix(tc.name, "astral") {
				assert.True(t, strings.HasSuffix(*note.Text, "\U0001F600"), "a surrogate pair was split")
			}
		})
	}
}

// 編集 (Update) でも同じ長さで切る。
func TestUpdateRemoteNote_ClipsRemoteTextToUpstreamLength(t *testing.T) {
	r, _, _ := newProhibitedWordsUpdateResolver(t, nil)
	got, err := r.UpdateRemoteNote(remoteNoteJSON(t, "https://remote.example/notes/n1", map[string]any{"_misskey_content": strings.Repeat("b", 9000)}), "")
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, 8192, utf16Len(*got.Text))
}

// 禁止語は切る前の全文で判定する (本家 ApNoteService は全文で
// checkProhibitedWordsContain を見てから NoteCreateService で切る)。保存される
// 8192 単位より後ろにある禁止語でも受け取らない。
func TestIngestNote_ProhibitedWordBeyondClipIsStillRejected(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, []string{"forbidden"})
	body := remoteNoteJSON(t, "https://remote.example/notes/tail-word", map[string]any{
		"_misskey_content": strings.Repeat("a", 9000) + " forbidden",
	})
	note, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Nil(t, note, "a note with a prohibited word past the clip point was stored")
	assert.Empty(t, noteRepo.Notes)
}

// 本文から拾う hashtag も全文から拾う。切り口をまたぐタグを途中までで拾わない。
func TestIngestNote_HashtagAcrossClipIsNotTruncated(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"), &stubFetcher{body: []byte(sampleActor)}, idGen)
	// 8187 + " #foo" で 8192 単位。切り口は `#foo` の直後。
	text := strings.Repeat("a", 8187) + " #foobar"
	note, err := r.IngestNote(remoteNoteJSON(t, "https://remote.example/notes/tag-cut", map[string]any{"_misskey_content": text}))
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(*note.Text, "#foo"), "precondition: the clip point is right after #foo")
	assert.NotContains(t, []string(note.Tags), "foo", "a tag cut at the clip point was extracted")
	assert.Contains(t, []string(note.Tags), "foobar")
}

// 連合のルールも切る前の全文で判定する。切り口より後ろにだけ当たるパターンでも
// 受け取らない。
func TestIngestNote_RulePatternBeyondClipIsStillApplied(t *testing.T) {
	svc, hits := newRules(t, &model.FederationRule{ID: "r1", Patterns: model.StringArray{"buy now"}, Reject: true})
	r, noteRepo, _ := newRuleResolver(t, svc)
	body := remoteNoteJSON(t, "https://remote.example/notes/tail-rule", map[string]any{
		"_misskey_content": strings.Repeat("a", 9000) + " buy now",
	})
	note, _, err := r.IngestNoteWithCreated(body, "")
	require.NoError(t, err)
	assert.Nil(t, note, "a rule matching past the clip point was not applied")
	assert.Empty(t, noteRepo.Notes)
	require.Len(t, hits.hits, 1)
}

// Update の禁止語も全文で判定する。末尾にだけ禁止語がある長文への差し替えは
// 反映しない。
func TestUpdateRemoteNote_ProhibitedWordBeyondClipIsStillRejected(t *testing.T) {
	r, noteRepo, _ := newProhibitedWordsUpdateResolver(t, []string{"forbidden"})
	got, err := r.UpdateRemoteNote(remoteNoteJSON(t, "https://remote.example/notes/n1", map[string]any{
		"_misskey_content": strings.Repeat("a", 9000) + " forbidden",
	}), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "original", *noteRepo.Notes["n1"].Text)
	assert.Empty(t, noteRepo.UpdateFieldsCalls)
}

// Update の本文からの hashtag も全文から拾う。
func TestUpdateRemoteNote_HashtagAcrossClipIsNotTruncated(t *testing.T) {
	r, noteRepo, _ := newProhibitedWordsUpdateResolver(t, nil)
	_, err := r.UpdateRemoteNote(remoteNoteJSON(t, "https://remote.example/notes/n1", map[string]any{
		"_misskey_content": strings.Repeat("a", 8187) + " #foobar",
	}), "")
	require.NoError(t, err)
	tags := []string(noteRepo.Notes["n1"].Tags)
	assert.Contains(t, tags, "foobar")
	assert.NotContains(t, tags, "foo")
}
