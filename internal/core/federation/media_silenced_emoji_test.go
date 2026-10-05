package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

const blobEmojiTag = `"tag": [{"type":"Emoji","name":":blob:","icon":{"type":"Image","url":"https://remote.example/emoji/blob.png"}}]`

// メディアサイレンスのホストの投稿は、カスタム絵文字を載せない (upstream の
// NoteCreateService と同じ)。絵文字の行は upstream も作るので作る。
func TestIngestNote_MediaSilencedHostDropsEmojis(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checker *stubMediaSilenced
		want    model.StringArray
	}{
		{"silenced", &stubMediaSilenced{hosts: map[string]bool{"remote.example": true}}, model.StringArray{}},
		{"other host", &stubMediaSilenced{hosts: map[string]bool{"elsewhere.example": true}}, model.StringArray{"blob"}},
		{"not wired", nil, model.StringArray{"blob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := newRuleResolver(t, nil)
			emojiRepo := testutil.NewMockEmojiRepository()
			r.SetEmojiRepo(emojiRepo)
			if tc.checker != nil {
				r.SetMediaSilencedHostChecker(*tc.checker)
			}
			note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "hi :blob:", `+blobEmojiTag)), "")
			require.NoError(t, err)
			require.NotNil(t, note.Emojis, "never nil (note.emojis is NOT NULL)")
			assert.Equal(t, tc.want, note.Emojis)
			assert.Len(t, emojiRepo.Emojis, 1, "the emoji row is still stored")
		})
	}
}

// 編集の取り込みでも落とす。前に載っていた絵文字も外れる。
func TestUpdateRemoteNote_MediaSilencedHostDropsEmojis(t *testing.T) {
	r, noteRepo, emojiRepo := newProhibitedWordsUpdateResolver(t, nil)
	r.SetMediaSilencedHostChecker(stubMediaSilenced{hosts: map[string]bool{"remote.example": true}})
	noteRepo.Notes["n1"].Emojis = model.StringArray{"old"}
	got, err := r.UpdateRemoteNote([]byte(`{"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"edited :blob:", `+blobEmojiTag+`}`), "")
	require.NoError(t, err)
	require.NotNil(t, got.Emojis)
	assert.Empty(t, got.Emojis)
	assert.Empty(t, noteRepo.Notes["n1"].Emojis)
	assert.NotNil(t, noteRepo.Notes["n1"].Emojis)
	assert.Len(t, emojiRepo.Emojis, 1)
}
