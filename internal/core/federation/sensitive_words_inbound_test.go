package federation_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSensitiveWords implements federation.SensitiveWordsProvider.
type stubSensitiveWords struct{ words []string }

func (s stubSensitiveWords) SensitiveWords() []string { return s.words }

// blockerWithSensitiveWords is a HostBlockChecker that also exposes
// sensitiveWords, the way core/instance.Service does in production.
type blockerWithSensitiveWords struct {
	stubHostBlocker
	words []string
}

func (b *blockerWithSensitiveWords) SensitiveWords() []string { return b.words }

const followersOnlyAudience = `"to": ["https://remote.example/users/alice/followers"]`

// センシティブワードに当たるリモートの public の投稿は home にする (本家は
// リモートの投稿も NoteCreateService.create を通すので、ローカルと同じ降格が
// 掛かる)。判定はローカルと同じく欄ごとで、URL を表示される形に直したものも見る。
func TestIngestNote_SensitiveWordsDemotePublicToHome(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		words []string
		want  model.NoteVisibility
	}{
		{name: "text", body: remoteNoteBody(`"content": "a spoiler here"`), words: []string{"spoiler"}, want: model.NoteVisibilityHome},
		{name: "cw", body: remoteNoteBody(`"content": "clean", "summary": "spoiler"`), words: []string{"spoiler"}, want: model.NoteVisibilityHome},
		{name: "harmless cw does not hide the text", body: remoteNoteBody(`"content": "a spoiler here", "summary": "cw"`), words: []string{"spoiler"}, want: model.NoteVisibilityHome},
		{name: "normalized URL", body: remoteNoteBody(`"content": "x", "_misskey_content": "see https://%73ensitive.example/"`), words: []string{"sensitive.example"}, want: model.NoteVisibilityHome},
		{name: "normalized URL in cw", body: remoteNoteBody(`"content": "clean", "summary": "https://%73ensitive.example/"`), words: []string{"sensitive.example"}, want: model.NoteVisibilityHome},
		{name: "no match", body: remoteNoteBody(`"content": "perfectly fine"`), words: []string{"spoiler"}, want: model.NoteVisibilityPublic},
		{name: "empty list", body: remoteNoteBody(`"content": "a spoiler here"`), words: nil, want: model.NoteVisibilityPublic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, noteRepo := newProhibitedWordsResolver(t, nil)
			r.SetSensitiveWordsProvider(stubSensitiveWords{words: tc.words})

			note, created, err := r.IngestNoteWithCreated([]byte(tc.body), "")
			require.NoError(t, err)
			require.NotNil(t, note)
			assert.True(t, created)
			assert.Equal(t, tc.want, note.Visibility)
			require.Len(t, noteRepo.Notes, 1)
		})
	}
}

// public でない投稿の可視性は変えない (本家も public のときだけ降格する)。
func TestIngestNote_SensitiveWordsKeepNonPublic(t *testing.T) {
	r, _ := newProhibitedWordsResolver(t, nil)
	r.SetSensitiveWordsProvider(stubSensitiveWords{words: []string{"spoiler"}})

	body := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		` + followersOnlyAudience + `,
		"content": "a spoiler here"
	}`
	note, _, err := r.IngestNoteWithCreated([]byte(body), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, model.NoteVisibilityFollowers, note.Visibility)
}

// 本番は hostBlocker (core/instance.Service) から読む。
func TestIngestNote_SensitiveWordsComeFromHostBlocker(t *testing.T) {
	r, _ := newProhibitedWordsResolver(t, nil)
	assert.False(t, r.HasSensitiveWordsSource())
	r.SetHostBlockChecker(&stubHostBlocker{})
	assert.False(t, r.HasSensitiveWordsSource(), "SensitiveWords を持たない hostBlocker では読めない")
	r.SetHostBlockChecker(&blockerWithSensitiveWords{words: []string{"spoiler"}})
	assert.True(t, r.HasSensitiveWordsSource())

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "a spoiler here"`)), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, model.NoteVisibilityHome, note.Visibility)
}

func TestHasSensitiveWordsSource_ExplicitProvider(t *testing.T) {
	r, _ := newProhibitedWordsResolver(t, nil)
	r.SetSensitiveWordsProvider(stubSensitiveWords{})
	assert.True(t, r.HasSensitiveWordsSource())
}

func sensitiveUpdateBody(extra string) string {
	return `{"@context":"https://www.w3.org/ns/activitystreams",
		"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice",
		` + extra + `}`
}

// 編集 (Update) の取り込みでも、編集した本文 / CW がセンシティブワードに当たれば
// public を home にする。狭める方向だけで、public 以外は変えず、home を public に
// 戻すこともしない。
func TestUpdateRemoteNote_SensitiveWordsDemote(t *testing.T) {
	tests := []struct {
		name  string
		from  model.NoteVisibility
		extra string
		want  model.NoteVisibility
	}{
		{name: "public text hit", from: model.NoteVisibilityPublic, extra: `"content":"now a spoiler"`, want: model.NoteVisibilityHome},
		{name: "public cw hit via normalized URL", from: model.NoteVisibilityPublic, extra: `"content":"clean","summary":"https://%73ensitive.example/"`, want: model.NoteVisibilityHome},
		{name: "public no hit", from: model.NoteVisibilityPublic, extra: `"content":"perfectly fine"`, want: model.NoteVisibilityPublic},
		{name: "followers untouched", from: model.NoteVisibilityFollowers, extra: `"content":"now a spoiler"`, want: model.NoteVisibilityFollowers},
		{name: "home is not upgraded", from: model.NoteVisibilityHome, extra: `"content":"perfectly fine"`, want: model.NoteVisibilityHome},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, noteRepo, _ := newProhibitedWordsUpdateResolver(t, nil)
			noteRepo.Notes["n1"].Visibility = tc.from
			r.SetSensitiveWordsProvider(stubSensitiveWords{words: []string{"spoiler", "sensitive.example"}})

			got, err := r.UpdateRemoteNote([]byte(sensitiveUpdateBody(tc.extra)), "")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Visibility)
			// 書き込む列にも載ること (戻り値だけ変えて DB に書かない形を落とす)。
			require.NotEmpty(t, noteRepo.UpdateFieldsCalls)
			v, written := noteRepo.UpdateFieldsCalls[0]["visibility"]
			if tc.want != tc.from {
				assert.True(t, written)
				assert.Equal(t, tc.want, v)
			} else {
				assert.False(t, written, "可視性を変えないときは列を書かない")
			}
		})
	}
}
