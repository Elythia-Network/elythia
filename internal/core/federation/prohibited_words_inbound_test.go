package federation_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubProhibitedWords implements federation.ProhibitedWordsProvider.
type stubProhibitedWords struct{ words []string }

func (s stubProhibitedWords) ProhibitedWords() []string { return s.words }

// blockerWithWords is a HostBlockChecker that also exposes prohibitedWords,
// the way core/instance.Service does in production (router.go は同じ実体を
// hostBlocker として配線する)。
type blockerWithWords struct {
	stubHostBlocker
	words []string
}

func (b *blockerWithWords) ProhibitedWords() []string { return b.words }

func newProhibitedWordsResolver(t *testing.T, words []string) (*federation.Resolver, *testutil.MockNoteRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(userRepo, noteRepo, activitypub.NewURLBuilder("https://example.com"),
		&stubFetcher{body: []byte(sampleActor)}, idGen)
	if words != nil {
		r.SetProhibitedWordsProvider(stubProhibitedWords{words: words})
	}
	return r, noteRepo
}

func remoteNoteBody(extra string) string {
	return `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		` + extra + `
	}`
}

// 禁止語 (meta.prohibitedWords) は inbound の note 取り込みにも掛かること。
// upstream ApNoteService.ts:203 が `checkProhibitedWordsContain` を添付 /
// ユーザー登録の前に呼ぶのと同じ位置づけ。**呼び出し側が retry しないよう
// error ではなく nil note で落とす** (upstream も InboxProcessorService が
// catch して ack する = retry しない)。
func TestIngestNote_DropsProhibitedWordInText(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, []string{"forbidden"})

	note, created, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "this is forbidden stuff"`)), "")
	require.NoError(t, err, "retry キューへ戻さないので error にはしない")
	assert.Nil(t, note)
	assert.False(t, created)
	assert.Empty(t, noteRepo.Notes, "禁止語を含む note を保存してはいけない")
}

// CW も検査対象 (upstream の checkProhibitedWordsContain は cw / text /
// pollChoices の 3 つを見る)。
func TestIngestNote_DropsProhibitedWordInCW(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, []string{"forbidden"})

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "clean", "summary": "forbidden warning"`)), "")
	require.NoError(t, err)
	assert.Nil(t, note)
	assert.Empty(t, noteRepo.Notes)
}

// 投票の選択肢も検査対象。
func TestIngestNote_DropsProhibitedWordInPollChoice(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, []string{"forbidden"})

	body := remoteNoteBody(`"content": "vote please", "oneOf": [{"type":"Note","name":"ok"},{"type":"Note","name":"forbidden"}]`)
	note, _, err := r.IngestNoteWithCreated([]byte(body), "")
	require.NoError(t, err)
	assert.Nil(t, note)
	assert.Empty(t, noteRepo.Notes)
}

// `/regex/flags` 形式もローカル投稿経路と同じに解釈されること (判定は
// internal/misc/keyword を共有しているので、素朴な部分文字列一致に落ちて
// いないことを固定する)。
func TestIngestNote_ProhibitedWordsSupportRegexFilters(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, []string{"/ba[dn]word/i"})

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "a BANWORD here"`)), "")
	require.NoError(t, err)
	assert.Nil(t, note)
	assert.Empty(t, noteRepo.Notes)
}

// 禁止語に当たらない note は従来どおり取り込まれること (境界の反対側)。
func TestIngestNote_AcceptsWhenNoProhibitedWordMatches(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, []string{"forbidden"})

	note, created, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "perfectly fine"`)), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.True(t, created)
	assert.Len(t, noteRepo.Notes, 1)
}

// provider を明示配線しなくても、既に配線済みの hostBlocker が
// ProhibitedWordsProvider を実装していれば判定が効くこと。本番 (router.go) は
// hostBlocker に core/instance.Service を渡すので、この経路が実際に使われる。
func TestIngestNote_ProhibitedWordsComeFromHostBlocker(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, nil)
	r.SetHostBlockChecker(&blockerWithWords{words: []string{"forbidden"}})

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "forbidden"`)), "")
	require.NoError(t, err)
	assert.Nil(t, note)
	assert.Empty(t, noteRepo.Notes)
}

// 読み取り元が無い / 禁止語が空なら従来どおり素通り (fail-open)。他の meta
// ゲートと同じベストエフォート方針。
func TestIngestNote_NoProhibitedWordsSourceIngestsNormally(t *testing.T) {
	r, noteRepo := newProhibitedWordsResolver(t, nil)
	// prohibitedWords を持たない hostBlocker (= 既存の stub) でも落ちないこと。
	r.SetHostBlockChecker(&stubHostBlocker{})

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "forbidden"`)), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Len(t, noteRepo.Notes, 1)
}

// --- Update 経路 -------------------------------------------------------------

func newProhibitedWordsUpdateResolver(t *testing.T, words []string) (*federation.Resolver, *testutil.MockNoteRepository, *testutil.MockEmojiRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	original := "original"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host, Text: &original}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(userRepo, noteRepo, activitypub.NewURLBuilder("https://example.com"),
		&stubFetcher{}, idGen)
	r.SetEmojiRepo(emojiRepo)
	r.SetProhibitedWordsProvider(stubProhibitedWords{words: words})
	return r, noteRepo, emojiRepo
}

// 編集でも禁止語を掛けること。掛けないと「無害な note を作ってから Update で
// 差し替える」で create 側の判定を素通りできる。
func TestUpdateRemoteNote_DropsProhibitedWord(t *testing.T) {
	r, noteRepo, emojiRepo := newProhibitedWordsUpdateResolver(t, []string{"forbidden"})

	body := `{"@context":"https://www.w3.org/ns/activitystreams",
		"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice",
		"content":"now forbidden","summary":"edited cw",
		"tag":[{"type":"Emoji","name":":evil:","icon":{"type":"Image","url":"https://remote.example/e.png"}}]}`
	got, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	require.NotNil(t, got)

	require.NotNil(t, noteRepo.Notes["n1"].Text)
	assert.Equal(t, "original", *noteRepo.Notes["n1"].Text, "禁止語を含む編集を反映してはいけない")
	assert.Nil(t, noteRepo.Notes["n1"].CW)
	assert.Empty(t, noteRepo.UpdateFieldsCalls, "note の列は 1 つも書かない")
	// **note の列だけの話ではない。** 判定が upsertEmojis より後ろにあると
	// 「本文は更新しないが絵文字だけ差し替わる」形になる。
	assert.Empty(t, emojiRepo.Emojis, "弾いた Update で emoji 行を作ってはいけない")
	// in-memory の戻り値も汚染しない (caller が stream へ流す可能性がある)。
	require.NotNil(t, got.Text)
	assert.Equal(t, "original", *got.Text)
}

// CW だけに禁止語がある編集も弾くこと。
func TestUpdateRemoteNote_DropsProhibitedWordInCW(t *testing.T) {
	r, noteRepo, _ := newProhibitedWordsUpdateResolver(t, []string{"forbidden"})

	body := `{"@context":"https://www.w3.org/ns/activitystreams",
		"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice",
		"content":"clean body","summary":"forbidden cw"}`
	_, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	assert.Empty(t, noteRepo.UpdateFieldsCalls)
	assert.Equal(t, "original", *noteRepo.Notes["n1"].Text)
}

// 禁止語に当たらない編集は従来どおり反映されること (境界の反対側)。
func TestUpdateRemoteNote_AppliesWhenNoProhibitedWordMatches(t *testing.T) {
	r, noteRepo, _ := newProhibitedWordsUpdateResolver(t, []string{"forbidden"})

	body := `{"@context":"https://www.w3.org/ns/activitystreams",
		"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice",
		"content":"edited content","summary":"edited cw"}`
	got, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "edited content", *got.Text)
	require.NotNil(t, got.CW)
	assert.Equal(t, "edited cw", *got.CW)
	assert.NotEmpty(t, noteRepo.UpdateFieldsCalls)
}

// 編集で本文が変わらなくても、**既存の本文**が禁止語に当たるなら弾くこと
// (管理者が後から禁止語を足した後の編集で、tag / 添付だけ差し替えられる形)。
func TestUpdateRemoteNote_ChecksExistingTextWhenContentAbsent(t *testing.T) {
	r, noteRepo, emojiRepo := newProhibitedWordsUpdateResolver(t, []string{"original"})

	body := `{"@context":"https://www.w3.org/ns/activitystreams",
		"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice",
		"tag":[{"type":"Emoji","name":":evil:","icon":{"type":"Image","url":"https://remote.example/e.png"}}]}`
	_, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	assert.Empty(t, noteRepo.UpdateFieldsCalls)
	assert.Empty(t, emojiRepo.Emojis)
}
