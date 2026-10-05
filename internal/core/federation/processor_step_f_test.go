package federation_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/pgarray"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullProcessor builds a processor with reactionService / noteDeleteService
// wired in (Step F handlers depend on these). It returns the constructed
// processor and the underlying repos so tests can inspect/mutate state.
type fullProcessorEnv struct {
	processor    *federation.Processor
	userRepo     *testutil.MockUserRepository
	noteRepo     *testutil.MockNoteRepository
	reactionRepo *testutil.MockNoteReactionRepository
	emojiRepo    *testutil.MockEmojiRepository
	resolver     *federation.Resolver
}

func newFullProcessor(t *testing.T, fetcherBody string) *fullProcessorEnv {
	t.Helper()
	return newFullProcessorFetcher(t, &stubFetcher{body: []byte(fetcherBody)})
}

// newFullProcessorDocs is newFullProcessor with a per-URI fetcher, for tests
// that need both an actor document and a note document (#2751)。
func newFullProcessorDocs(t *testing.T, docs map[string]string) *fullProcessorEnv {
	t.Helper()
	return newFullProcessorFetcher(t, &docFetcher{docs: docs})
}

func newFullProcessorFetcher(t *testing.T, fetcher federation.HTTPFetcher) *fullProcessorEnv {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	reactionRepo := testutil.NewMockNoteReactionRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(userRepo, noteRepo, urls, fetcher, idGen)
	resolver.SetEmojiRepo(emojiRepo)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	reactionSvc := corereaction.NewService(noteRepo, reactionRepo, emojiRepo, followingRepo, idGen)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	p := federation.NewProcessor(resolver, followingSvc, reactionSvc, deleteSvc, userRepo, noteRepo)
	return &fullProcessorEnv{processor: p, userRepo: userRepo, noteRepo: noteRepo, reactionRepo: reactionRepo, emojiRepo: emojiRepo, resolver: resolver}
}

const noteCreateBody = `{
	"type": "Create",
	"actor": "https://remote.example/users/alice",
	"object": {
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hi",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}
}`

func TestProcess_CreateNote(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	require.NoError(t, env.processor.Process([]byte(noteCreateBody)))
	// 取り込まれた note は uri で検索できるはず
	found, err := env.noteRepo.FindByURI("https://remote.example/notes/n1")
	require.NoError(t, err)
	require.NotNil(t, found.Text)
	assert.Equal(t, "hi", *found.Text)
}

func TestProcess_CreateActorError(t *testing.T) {
	env := newFullProcessor(t, "{not json")
	err := env.processor.Process([]byte(noteCreateBody))
	assert.Error(t, err)
}

func TestProcess_CreateNoteIngestError(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// object に id が無いケースは attributedTo を actor から補って (#1560) もなお
	// IngestNoteWithCreated の ID 空チェックで ErrInvalidNote になり Process が
	// error を surface する。
	body := []byte(`{"type":"Create","actor":"https://remote.example/users/alice","object":{"content":"no id"}}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

// --- Like ---------------------------------------------------------------------

func TestProcess_LikeHappyPath(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// ローカルノート bob/n1 を登録
	env.noteRepo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     "bob",
		Visibility: model.NoteVisibilityPublic,
	}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "👍"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Len(t, env.reactionRepo.Reactions, 1)
}

func TestProcess_LikeFromObjectURI(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Len(t, env.reactionRepo.Reactions, 1)
}

func TestProcess_LikeAlreadyReacted(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "👍"
	}`)
	require.NoError(t, env.processor.Process(body))
	// 2 回目: ErrAlreadyReacted は飲み込まれる
	require.NoError(t, env.processor.Process(body))
}

// TestProcess_LikeDuplicateRaceSwallowed: reaction_service の Create が
// repository.ErrDuplicateReaction (= DB unique violation by race) を
// 受けても、service が ErrAlreadyReacted に変換し handleLike が swallow
// するので activity が ack される。AP idempotent semantic を保つ (#1186)。
func TestProcess_LikeDuplicateRaceSwallowed(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	// reactionRepo.Create を強制的に ErrDuplicateReaction で失敗させる
	// (= FindByPair 後に並行 process が同 user×note に Create を入れた
	//   race を simulate)。
	env.reactionRepo.CreateErr = repository.ErrDuplicateReaction
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "👍"
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_LikeUnknownTarget(t *testing.T) {
	// best-effort ack semantic (#1183): target note の resolve 失敗
	// (= stubFetcher が actor JSON を返すので Note として parse できず
	// `ErrInvalidNote` で permanent fail) は activity を ack して reaction
	// record を作らない。retry サイクルに乗せない。
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/missing"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Empty(t, env.reactionRepo.Reactions, "permanent target resolve fail は reaction を作らない")
}

func TestProcess_LikeNoReactionService(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_LikeBadObject(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": 42
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

func TestProcess_LikeActorError(t *testing.T) {
	// best-effort ack (#1183): actor JSON parse 失敗 (ErrInvalidActor) は
	// permanent fail として ack。reactor が居なければ reaction も作れない
	// ので skip = noop。
	env := newFullProcessor(t, "{not json")
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Empty(t, env.reactionRepo.Reactions, "permanent actor resolve fail は reaction を作らない")
}

// --- Like content / _misskey_reaction (ported from nekonoverse handler_like) --

func TestProcess_LikeWithContent(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "⭐"
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1)
	for _, r := range env.reactionRepo.Reactions {
		assert.Equal(t, "⭐", r.Reaction)
	}
}

// #1948-21: content/_misskey_reaction が無い Like は activity.name を reaction の
// fallback に使う (upstream `_misskey_reaction ?? content ?? name`)。一部の
// Mastodon-fork / Pleroma 系 EmojiReact は shortcode を name に乗せる。
func TestProcess_LikeWithName(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"name": "🐧"
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1)
	for _, r := range env.reactionRepo.Reactions {
		assert.Equal(t, "🐧", r.Reaction, "content/_misskey_reaction 無しなら name を reaction に使う (#1948-21)")
	}
}

func TestProcess_LikeWithMisskeyReaction(t *testing.T) {
	// _misskey_reaction は content より優先される
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "Like",
		"_misskey_reaction": "😀"
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1)
	for _, r := range env.reactionRepo.Reactions {
		assert.Equal(t, "😀", r.Reaction)
	}
}

func TestProcess_LikeWithCustomEmojiTag(t *testing.T) {
	// #459: カスタム絵文字リアクションが Tag 付きで届いたら emoji table に
	// upsert されるべき。Misskey TS の apNoteService.extractEmojis 相当。
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"_misskey_reaction": ":custom@remote.example:",
		"tag": [{
			"type": "Emoji",
			"id": "https://remote.example/emojis/custom",
			"name": ":custom:",
			"icon": {"type": "Image", "url": "https://remote.example/files/custom.png"}
		}]
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1)
	// emoji table に :custom: が remote.example host で upsert されている
	host := "remote.example"
	emojis, err := env.emojiRepo.FindManyByNamesAndHost([]string{"custom"}, &host)
	require.NoError(t, err)
	require.Len(t, emojis, 1, "custom emoji should be upserted from Like.tag")
	assert.Equal(t, "https://remote.example/files/custom.png", emojis[0].OriginalURL)
}

func TestProcess_LikeEmptyContent(t *testing.T) {
	// content が無い場合は reaction service が FallbackReaction (❤) に正規化する
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1)
	for _, r := range env.reactionRepo.Reactions {
		assert.Equal(t, "\u2764", r.Reaction)
	}
}

// --- Announce ----------------------------------------------------------------

func TestProcess_AnnounceHappyPath(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	// 取り込まれた renote が 1 件存在するはず
	var renotes int
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == "n1" {
			renotes++
		}
	}
	assert.Equal(t, 1, renotes)
}

func TestProcess_AnnounceDuplicate(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	// 2 回目は重複検出で no-op
	require.NoError(t, env.processor.Process(body))
	var renotes int
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == "n1" {
			renotes++
		}
	}
	assert.Equal(t, 1, renotes)
}

func TestProcess_AnnounceUnknownTarget(t *testing.T) {
	// best-effort ack (#1183): target resolve 失敗は permanent として skip。
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Announce",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/missing"
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_AnnounceActorError(t *testing.T) {
	// best-effort ack (#1183): actor resolve 失敗 (ErrInvalidActor) は
	// permanent として skip。
	env := newFullProcessor(t, "{not json")
	body := []byte(`{
		"type": "Announce",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_AnnounceBadObject(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Announce",
		"actor": "https://remote.example/users/alice",
		"object": 42
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

// upstream Misskey #17308 (= 2026.5.0 fix / triage #1002): relay actor からの
// Announce は renote として保存せず、target を fanoutHook 経由で stream publish
// する。RelayActorChecker が true を返すケースで renote 件数が増えず、かつ
// fanoutHook が呼ばれることを実証する。
//
// stub の relayActorChecker と fanoutHook を inject して挙動を確認。
type stubRelayActorChecker struct{ relayActorURI string }

func (s *stubRelayActorChecker) IsRelayActor(u *model.User) bool {
	if u == nil || u.URI == nil {
		return false
	}
	return *u.URI == s.relayActorURI
}

// recordingFanoutHook は publishRelayDeliveredNote が safeGoFedHook 経由で
// async に call することに対応するため channel ベースで完了を待てる stub。
type recordingFanoutCall struct {
	noteID string
	author string
}

type recordingFanoutHook struct {
	calls chan recordingFanoutCall
}

func newRecordingFanoutHook() *recordingFanoutHook {
	return &recordingFanoutHook{calls: make(chan recordingFanoutCall, 16)}
}

func (h *recordingFanoutHook) OnNoteCreated(note *model.Note, author *model.User) {
	authorID := ""
	if author != nil {
		authorID = author.ID
	}
	noteID := ""
	if note != nil {
		noteID = note.ID
	}
	h.calls <- recordingFanoutCall{noteID: noteID, author: authorID}
}

func TestProcess_AnnounceFromRelay_PublishesTargetWithoutRenote(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// announcer alice を relay actor として扱う
	checker := &stubRelayActorChecker{relayActorURI: "https://remote.example/users/alice"}
	env.processor.SetRelayActorChecker(checker)
	// fanoutHook を仕込んで呼び出しを記録 (= safeGoFedHook で async に呼ばれる
	// ので channel 受信で待ち合わせる)
	hook := newRecordingFanoutHook()
	env.processor.SetFanoutHook(hook)
	// target note (= 原投稿) と原投稿者 bob をローカル DB に
	env.noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
	}
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/relay-1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))

	// renote は作られないこと
	var renotes int
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == "n1" {
			renotes++
		}
	}
	assert.Equal(t, 0, renotes, "relay 由来 Announce は renote を作らない")

	// fanoutHook が target (= n1) を author=bob で publish した
	select {
	case call := <-hook.calls:
		assert.Equal(t, "n1", call.noteID, "publish 対象は target note")
		assert.Equal(t, "bob", call.author, "author は relay actor でなく原投稿者")
	case <-time.After(2 * time.Second):
		t.Fatal("fanoutHook was not called within timeout")
	}
}

// RelayActorChecker 未設定 (= 旧挙動) では announce は従来通り renote を作る。
// 既存 TestProcess_AnnounceHappyPath と同等だが「checker 未配線でも regress
// しないこと」を明示する regression guard。
func TestProcess_AnnounceWithoutRelayChecker_StillCreatesRenote(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// SetRelayActorChecker を呼ばない (= 旧挙動 / nil-checker fallback)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/no-checker",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))

	var renotes int
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == "n1" {
			renotes++
		}
	}
	assert.Equal(t, 1, renotes, "checker 未設定なら従来 path で renote 1 件作成")
}

func TestProcess_AnnounceIncrementsRenoteCount(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a2",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Equal(t, int16(1), env.noteRepo.Notes["n1"].RenoteCount)
}

// upstream NoteCreateService の incRenoteCount 条件を inbound Announce にも
// 適用する (#2283)。boost 対象が announcer 本人の note なら加算しない。
func TestProcess_AnnounceSelfBoostDoesNotIncrementRenoteCount(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// announcer を既知 ID で事前登録し、その本人の note を boost 対象にする。
	env.userRepo.Users["ualice"] = &model.User{
		ID: "ualice", Username: "alice",
		Host: strPtr("remote.example"), URI: strPtr("https://remote.example/users/alice"),
	}
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "ualice", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/self",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Equal(t, int16(0), env.noteRepo.Notes["n1"].RenoteCount,
		"自己 boost は renoteCount を増やさない")
}

// --- Delete ------------------------------------------------------------------

// TestProcess_SingletonArrayActivity_Foundkey verifies that a valid AP
// Delete activity wrapped in a single-element JSON array (= Foundkey-style
// send format) is unwrapped and processed normally instead of failing
// with "activity missing actor" (#1185).
//
// 実機データ (UDS 本番) で 26 件の missing-actor failure がすべて 1 つの
// Foundkey instance 由来だったケースの regression guard。
func TestProcess_SingletonArrayActivity_Foundkey(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	uri := "https://remote.example/notes/n1"
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice-id", URI: &uri}
	aliceURI := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}

	// 1 要素 JSON array で wrap した payload (Foundkey-style)。
	body := []byte(`[
		{
			"type": "Delete",
			"actor": "https://remote.example/users/alice",
			"object": {"id": "https://remote.example/notes/n1", "type": "Tombstone"}
		}
	]`)
	require.NoError(t, env.processor.Process(body))

	// 中身が unwrap されて handleDelete が走った結果、n1 が削除される。
	_, err := env.noteRepo.FindByID("n1")
	assert.Error(t, err, "wrapped Delete activity should still remove the target note")
}

// TestProcess_MultiElementArray_NotUnwrapped: 2 要素 array は AS spec 外
// として現状は no-op で素通し → 既存挙動の missing-actor で fail する。
// 将来 batch 対応する余地を残すための保守的な挙動を pin する (#1185)。
func TestProcess_MultiElementArray_NotUnwrapped(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`[
		{"type":"Delete","actor":"https://remote.example/users/alice","object":{"id":"x"}},
		{"type":"Delete","actor":"https://remote.example/users/alice","object":{"id":"y"}}
	]`)
	err := env.processor.Process(body)
	assert.ErrorContains(t, err, "missing actor",
		"multi-element array should not be unwrapped; downstream still fails with missing actor")
}

func TestProcess_DeleteRemoteNote(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// alice 自身が著者であるリモートnoteを事前に登録
	uri := "https://remote.example/notes/n1"
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice-id", URI: &uri}
	// resolver が alice を作る前に DB に揃える: 上記 ResolveActor で生成される ID と
	// 一致させるため、alice を予め repo に置いておく
	aliceURI := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}

	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": {"id": "https://remote.example/notes/n1", "type": "Tombstone"}
	}`)
	require.NoError(t, env.processor.Process(body))
	_, err := env.noteRepo.FindByID("n1")
	assert.Error(t, err)
}

// Delete の object id に改行が付いていても届くこと。取り込み側は
// `trimWHATWGURL` 後の値を `note.uri` に入れるので、読み出し側が生値のままだと
// FindByURI が miss して「取り込めるが消せない」ノートになる (#2662)。
func TestProcess_DeleteRemoteNote_PaddedObjectID(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	uri := "https://remote.example/notes/n1"
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice-id", URI: &uri}
	aliceURI := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}

	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": {"id": "https://remote.example/notes/n1\n", "type": "Tombstone"}
	}`)
	require.NoError(t, env.processor.Process(body))
	_, err := env.noteRepo.FindByID("n1")
	assert.Error(t, err, "改行付き id の Delete でも削除されること")
}

// Delete アクティビティで返信が消されたとき、親ノートの repliesCount が
// デクリメントされることを end-to-end で確認する (issue #11, item 1)。
func TestProcess_DeleteRemoteReply_DecrementsParent(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	parentID := "parent1"
	env.noteRepo.Notes[parentID] = &model.Note{ID: parentID, UserID: "local-user", RepliesCount: 2}

	replyURI := "https://remote.example/notes/reply1"
	parent := parentID
	env.noteRepo.Notes["reply1"] = &model.Note{
		ID: "reply1", UserID: "alice-id", URI: &replyURI, ReplyID: &parent,
	}
	aliceURI := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}

	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": {"id": "https://remote.example/notes/reply1", "type": "Tombstone"}
	}`)
	require.NoError(t, env.processor.Process(body))

	assert.Equal(t, int16(1), env.noteRepo.Notes[parentID].RepliesCount)
}

func TestProcess_DeleteUnknownNote(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/missing"
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_DeleteFromNonAuthor(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	uri := "https://remote.example/notes/n1"
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "other", URI: &uri}
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1"
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

// fakeAccountDeleteEnqueuer records cascade-purge enqueue calls.
type fakeAccountDeleteEnqueuer struct{ ids []string }

func (f *fakeAccountDeleteEnqueuer) EnqueueDeleteAccount(p queue.DeleteAccountPayload) error {
	f.ids = append(f.ids, p.UserID)
	return nil
}

// seedRemoteAlice registers a known remote user (we only delete actors we
// already mirror; isActorDelete short-circuits unknown actors).
func seedRemoteAlice(env *fullProcessorEnv) string {
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	env.userRepo.Users["alice_remote"] = &model.User{ID: "alice_remote", Username: "alice", Host: &host, URI: &uri}
	return "alice_remote"
}

func TestProcess_DeleteActorSelfDelete(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	id := seedRemoteAlice(env)
	enq := &fakeAccountDeleteEnqueuer{}
	env.processor.SetAccountDeleteEnqueuer(enq)
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/users/alice"
	}`)
	require.NoError(t, env.processor.Process(body))

	// remote actor が tombstone され、cascade purge が 1 回 enqueue される。
	assert.True(t, env.userRepo.Users[id].IsDeleted, "actor must be marked deleted")
	require.Len(t, enq.ids, 1)
	assert.Equal(t, id, enq.ids[0])

	// AP retry: 2 回目は既に isDeleted なので二重 enqueue しない (idempotent)。
	require.NoError(t, env.processor.Process(body))
	assert.Len(t, enq.ids, 1)
}

func TestProcess_DeleteActorSelfDelete_NoEnqueuerStillTombstones(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	id := seedRemoteAlice(env)
	// enqueuer 未配線でも tombstone は行う (purge は degrade)。
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/users/alice"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.True(t, env.userRepo.Users[id].IsDeleted)
}

func TestProcess_DeleteActorSelfDelete_UnknownActorIgnored(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	enq := &fakeAccountDeleteEnqueuer{}
	env.processor.SetAccountDeleteEnqueuer(enq)
	// 一度も mirror していない actor の self-delete は何もせず ack (既存 guard)。
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/ghost",
		"object": "https://remote.example/users/ghost"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Empty(t, enq.ids)
}

func TestProcess_DeleteActorError(t *testing.T) {
	env := newFullProcessor(t, "{not json")
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1"
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

func TestProcess_DeleteBadObject(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": 42
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

// upstream Misskey #17167 (= 2026.5.0 fix / triage #1004): mentionLimit を
// 超える inbound Create は Resolver.IngestNote で ErrContainsTooManyMentions
// に転換され、handleCreate がそれを catch して nil 返却 (= queue retry 経路に
// 乗らず ack drop) する。triage doc の "role validation error の retry 防止"
// 要件を mk-go アーキで等価実装したことの end-to-end 証明。
func TestProcess_CreateNote_MentionLimitExceededIsDropped(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	var tagJSON string
	for i := 1; i <= 21; i++ { // 21 = DefaultMentionLimit (20) + 1
		if i > 1 {
			tagJSON += ","
		}
		tagJSON += `{"type": "Mention", "href": "https://example.com/users/u` + strconv.Itoa(i) + `"}`
	}
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/over",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "x",
			"to": ["https://www.w3.org/ns/activitystreams#Public"],
			"tag": [` + tagJSON + `]
		}
	}`)
	// 上限超え note は ack されるが drop される (queue は retry しない)。
	require.NoError(t, env.processor.Process(body))
	// note も DB に入っていないこと
	_, err := env.noteRepo.FindByURI("https://remote.example/notes/over")
	require.Error(t, err)
}

// upstream Misskey #17294 (= 2026.5.0 fix / triage #1001): 存在しない actor の
// Delete activity は ignore する。actor URI と object URI が一致する self-delete
// shape でローカル DB に同 URI の user が居ない場合、ResolveActor を呼ばずに
// nil を返して queue retry に乗らないようにする。
func TestProcess_DeleteActor_UnknownActorSkipped(t *testing.T) {
	// fetcher を空文字列にすることで、もし ResolveActor が呼ばれてしまった場合は
	// 失敗する設定にする。期待動作は「ResolveActor を一切呼ばずに nil を返す」。
	env := newFullProcessor(t, "")
	body := []byte(`{
		"type": "Delete",
		"actor": "https://gone.example/users/ghost",
		"object": "https://gone.example/users/ghost"
	}`)
	// 既存実装は ResolveActor 失敗で error を返していたが、本 fix で nil 返す。
	require.NoError(t, env.processor.Process(body))
}

// 既知 actor の self-delete は ResolveActor 後の (author.URI == targetURI) check
// で no-op になる従来 path を覆う。新 fix の影響で前段で早期 return しないこと
// (= actor が居る場合は通常の delete 処理に進むこと) を実証する。
func TestProcess_DeleteActor_KnownActorSelfDeleteStillNoop(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	aliceURI := "https://remote.example/users/alice"
	env.userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", URI: &aliceURI}
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/users/alice"
	}`)
	require.NoError(t, env.processor.Process(body))
}

// upstream Misskey #17340 (= 2026.5.0 fix / triage #999): activity.actor は
// 文字列 IRI でも embedded object でも受け入れる。Mastodon 系等から actor
// field が `{"id": "..."}` 形式で来た場合に actor missing で reject しないこと。
func TestProcess_ActorEmbeddedObject_NormalizedAndProcessed(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	body := []byte(`{
		"type": "Follow",
		"actor": {"id": "https://remote.example/users/alice", "type": "Person"},
		"object": "https://example.com/users/bob"
	}`)
	// normalizeActor が走り、Follow が正しく処理される (= actor missing で
	// errors.New("activity missing actor") にならない) ことを確認。
	require.NoError(t, env.processor.Process(body))
}

// noteDeleteSvc が nil の Processor では noteRepo.Delete が直接呼ばれる
func TestProcess_DeleteWithoutDeleteSvc(t *testing.T) {
	p, repo, _, noteRepo := newProcessor(t, aliceActor)
	uri := "https://remote.example/notes/n1"
	aliceURI := "https://remote.example/users/alice"
	repo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice-id", URI: &uri}
	body := []byte(`{
		"type": "Delete",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1"
	}`)
	require.NoError(t, p.Process(body))
	_, err := noteRepo.FindByID("n1")
	assert.Error(t, err)
}

// --- Undo Like ---------------------------------------------------------------

func TestProcess_UndoLikeHappyPath(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}

	// まず Like を作る
	likeBody := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "👍"
	}`)
	require.NoError(t, env.processor.Process(likeBody))
	require.Len(t, env.reactionRepo.Reactions, 1)

	// Undo Like
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Like",
			"object": "https://example.com/notes/n1"
		}
	}`)
	require.NoError(t, env.processor.Process(undo))
	assert.Empty(t, env.reactionRepo.Reactions)
}

func TestProcess_UndoLikeNotFound(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Like",
			"object": "https://example.com/notes/n1"
		}
	}`)
	require.NoError(t, env.processor.Process(undo))
}

func TestProcess_UndoLikeNoReactionService(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Like","object":"https://example.com/notes/n1"}
	}`)
	err := p.Process(undo)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_UndoLikeUnknownTarget(t *testing.T) {
	// best-effort ack (#1183): Undo(Like) の target resolve 失敗は ack。
	// 元 reaction record も無いはずなので idempotent と整合する。
	env := newFullProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Like","object":"https://example.com/notes/missing"}
	}`)
	require.NoError(t, env.processor.Process(undo))
}

func TestProcess_UndoLikeActorError(t *testing.T) {
	// best-effort ack (#1183): Undo(Like) の actor resolve 失敗は ack。
	env := newFullProcessor(t, "{not json")
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Like","object":"https://example.com/notes/n1"}
	}`)
	require.NoError(t, env.processor.Process(undo))
}

func TestProcess_UndoLikeBadObject(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Like","object":42}
	}`)
	err := env.processor.Process(undo)
	assert.Error(t, err)
}

// --- Undo Announce -----------------------------------------------------------

func TestProcess_UndoAnnounceHappyPath(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}

	announce := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	require.NoError(t, env.processor.Process(announce))

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Announce",
			"object": "https://example.com/notes/n1"
		}
	}`)
	require.NoError(t, env.processor.Process(undo))

	var renotes int
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == "n1" {
			renotes++
		}
	}
	assert.Equal(t, 0, renotes)
}

func TestProcess_UndoAnnounceNoMatch(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Announce",
			"object": "https://example.com/notes/n1"
		}
	}`)
	require.NoError(t, env.processor.Process(undo))
}

func TestProcess_UndoAnnounceActorError(t *testing.T) {
	// best-effort ack (#1183): Undo(Announce) の actor resolve 失敗は ack。
	env := newFullProcessor(t, "{not json")
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Announce","object":"https://example.com/notes/n1"}
	}`)
	require.NoError(t, env.processor.Process(undo))
}

func TestProcess_UndoAnnounceUnknownTarget(t *testing.T) {
	// best-effort ack (#1183): Undo(Announce) の target resolve 失敗は ack。
	env := newFullProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Announce","object":"https://example.com/notes/missing"}
	}`)
	require.NoError(t, env.processor.Process(undo))
}

func TestProcess_UndoAnnounceBadObject(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Announce","object":42}
	}`)
	err := env.processor.Process(undo)
	assert.Error(t, err)
}

// --- Update ------------------------------------------------------------------

// --- Note update (Step J) -----------------------------------------------------

// **著者を実在させないと attribution 検査が空振りする (#3037)。** 検査は
// `note.UserID` から著者を引いて `author.URI == actor` を見るので、居ない ID を
// 書いたフィクスチャは「引けなかった」経路を通る。fail-open だったころは
// それでも更新が通っていたため、**テストは検査を一度も踏んでいなかった**。
func TestProcess_UpdateNoteHappyPath(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	authorID := seedRemoteAlice(env)
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	original := "original"
	env.noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: authorID, UserHost: &host, Text: &original,
	}
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/n1",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "edited"
		}
	}`)
	require.NoError(t, env.processor.Process(body))
	got := env.noteRepo.Notes["n1"]
	require.NotNil(t, got.Text)
	assert.Equal(t, "edited", *got.Text)
}

func TestProcess_UpdateNote_NotFound(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/missing",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "edited"
		}
	}`)
	// 未取得ノートは silently ignore
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_UpdateNote_InvalidNoteIsAccepted(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// type は Note だが id が無いので ErrInvalidNote → 200 扱い
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note"
		}
	}`)
	require.NoError(t, env.processor.Process(body))
}

// updateFailNoteRepo causes UpdateFields to fail, exercising the
// non-ErrInvalidNote error path of handleUpdate.
type updateFailNoteRepo struct {
	*testutil.MockNoteRepository
}

func (r *updateFailNoteRepo) UpdateFields(_ string, _ map[string]any) error {
	return errors.New("update boom")
}

func TestProcess_UpdateNote_RepoErrorPropagates(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	mock := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	// 著者を実在させる (上の HappyPath と同じ理由)。
	aliceURI := "https://remote.example/users/alice"
	userRepo.Users["alice_remote"] = &model.User{
		ID: "alice_remote", Username: "alice", Host: &host, URI: &aliceURI,
	}
	mock.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice_remote", UserHost: &host,
	}
	noteRepo := &updateFailNoteRepo{MockNoteRepository: mock}
	emojiRepo := testutil.NewMockEmojiRepository()
	reactionRepo := testutil.NewMockNoteReactionRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	reactionSvc := corereaction.NewService(noteRepo, reactionRepo, emojiRepo, followingRepo, idGen)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	p := federation.NewProcessor(resolver, followingSvc, reactionSvc, deleteSvc, userRepo, noteRepo)

	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/n1",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "edited"
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
}

func TestProcess_UpdatePerson(t *testing.T) {
	// #1560: Update(Person) は actor を強制再 fetch して name だけでなく
	// isBot/isLocked 等の full profile を更新する (upstream updatePerson)。
	// fetcher が返す更新後 actor doc を反映することを検証する (旧実装は inline
	// object の name のみ反映していた)。
	updatedActor := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Service",
		"preferredUsername": "alice",
		"name": "Alice Updated",
		"manuallyApprovesFollowers": true,
		"inbox": "https://remote.example/users/alice/inbox"
	}`
	env := newFullProcessor(t, updatedActor)
	uri := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &uri, IsBot: false, IsLocked: false}
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/users/alice",
			"type": "Person",
			"name": "Alice Updated"
		}
	}`)
	require.NoError(t, env.processor.Process(body))
	updated := env.userRepo.Users["alice-id"]
	require.NotNil(t, updated.Name)
	assert.Equal(t, "Alice Updated", *updated.Name)
	// name 以外も full refresh されること (Service type -> isBot、
	// manuallyApprovesFollowers -> isLocked)。旧 name-only 実装では false のまま。
	assert.True(t, updated.IsBot, "Service actor type must update isBot (full refresh)")
	assert.True(t, updated.IsLocked, "manuallyApprovesFollowers must update isLocked (full refresh)")
}

// #1848: 配送 actor と異なる object.id の Update(Person) は、対象 actor の
// TTL-bypass 強制再取得 (amplification) を誘発させない。
func TestProcess_UpdatePerson_CrossActorRejected(t *testing.T) {
	// fetcher は呼ばれたら alice を "Hijacked" に書き換える doc を返す。gate が
	// 効いていれば ForceResolveActor 自体が呼ばれず、alice は再取得されない。
	refreshedDoc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Service",
		"preferredUsername": "alice",
		"name": "Hijacked",
		"inbox": "https://remote.example/users/alice/inbox"
	}`
	env := newFullProcessor(t, refreshedDoc)
	aliceURI := "https://remote.example/users/alice"
	origName := "Alice Original"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI, Name: &origName}
	// 配送 actor は別人 mallory、object.id は alice。
	body := []byte(`{
		"type": "Update",
		"actor": "https://evil.example/users/mallory",
		"object": {"id":"https://remote.example/users/alice","type":"Person","name":"Hijacked"}
	}`)
	require.NoError(t, env.processor.Process(body))
	updated := env.userRepo.Users["alice-id"]
	require.NotNil(t, updated.Name)
	assert.Equal(t, "Alice Original", *updated.Name, "別 actor の Update(Person) は対象を再取得・改変しない")
	assert.False(t, updated.IsBot, "別 actor の Update では full refresh も起きない")
}

func TestProcess_UpdateUnknownActor(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {"id":"https://remote.example/users/ghost","type":"Person","name":"X"}
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_UpdateNonPerson(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	uri := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &uri}
	// Note ではない非対応 type (Article 等) は no-op
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {"id":"https://remote.example/users/alice","type":"Article","name":"X"}
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_UpdateMissingObject(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice"
	}`)
	// object フィールド無し → peekObjectType は "" を返し Person 経路に入る
	// → readObjectString が "missing object" を返してエラー
	err := env.processor.Process(body)
	assert.Error(t, err)
}

func TestProcess_UpdateNoChanges(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	uri := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &uri}
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {"id":"https://remote.example/users/alice","type":"Person"}
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_UpdateObjectAsURI(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/users/alice"
	}`)
	require.NoError(t, env.processor.Process(body))
}

func TestProcess_UpdateBadObject(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": 42
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

// --- Reject ------------------------------------------------------------------

func TestProcess_RejectFollow(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	// local follower bob, remote followee alice (resolver で取り込まれる)
	bobURI := "https://example.com/users/bob"
	env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/bob",
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, env.processor.Process(body))
}

// `TestProcess_AcceptFollow_InnerActorEmbeddedObject` の Reject 版。
// (直前の `TestProcess_RejectFollow` は `require.NoError` だけの smoke test で
// follower 解決が壊れても緑のままなので、実際の解除はこちらで見る。)
// inner.actor が embedded object でも follow が解除されること。Reject は
// `readActorString(inner)` で follower を決めるので、型エラーを握るだけでなく
// 直後の `normalizeActor` (#999) も効いている (#2665)。
func TestProcess_RejectFollow_InnerActorEmbeddedObject(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	// **local user は本番と同じく uri NULL。** `userFromAPID` の
	// local-ID 分岐を通すために base URL を配線する (偽の uri を持たせて
	// FindByURI で通すと、本番に無い経路でしかテストしていないことになる)。
	p.SetLocalBaseURL("https://example.com")
	// local follower bob が remote followee alice をフォロー済み。
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice1"}

	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": {"id": "https://example.com/users/bob", "type": "Person"},
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, followingRepo.Followings, "embedded object の actor でも Reject が届くこと")
}

func TestProcess_RejectInvalidJSON(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": "not-json"
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

func TestProcess_RejectInnerNotFollow(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Like"}
	}`)
	err := env.processor.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_RejectActorError(t *testing.T) {
	env := newFullProcessor(t, "{not json")
	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Follow","actor":"https://example.com/users/bob"}
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

func TestProcess_RejectInnerMissingActor(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Follow"}
	}`)
	err := env.processor.Process(body)
	assert.Error(t, err)
}

func TestProcess_RejectUnknownFollower(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/ghost"
		}
	}`)
	require.NoError(t, env.processor.Process(body))
}

// --- Visibility derivation ----------------------------------------------------

func TestIngestNote_VisibilityHome(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	body := []byte(`{
		"id": "https://remote.example/notes/home",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "home",
		"to": ["https://remote.example/users/alice/followers"],
		"cc": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityHome, got.Visibility)
}

func TestIngestNote_VisibilityFollowers(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	body := []byte(`{
		"id": "https://remote.example/notes/fol",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "fol",
		"to": ["https://remote.example/users/alice/followers"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityFollowers, got.Visibility)
}

func TestIngestNote_VisibilitySpecified(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	body := []byte(`{
		"id": "https://remote.example/notes/spec",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "spec",
		"to": ["https://example.com/users/bob"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, got.Visibility)
}

// --- extractLocalNoteID via ResolveNote with extra path -----------------------

func TestResolveNote_LocalURIWithExtraSegment(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1"}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	got, err := r.ResolveNote("https://example.com/notes/n1/activity")
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

// noteCreateFailRepo causes Create on noteRepo to fail (for handleAnnounce).
type noteCreateFailRepo struct {
	*testutil.MockNoteRepository
}

func (r *noteCreateFailRepo) Create(_ *model.Note) error { return errors.New("create failed") }

// listRenotesFailRepo causes ListRenotesOf to fail (for handleUndoAnnounce).
type listRenotesFailRepo struct {
	*testutil.MockNoteRepository
}

func (r *listRenotesFailRepo) ListRenotesOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	return nil, errors.New("list failed")
}

// deleteFailNoteRepo causes Delete on noteRepo to fail (for handleUndoAnnounce).
type deleteFailNoteRepo struct {
	*testutil.MockNoteRepository
}

func (r *deleteFailNoteRepo) Delete(_ *model.Note) error { return errors.New("delete failed") }

// deleteFailReactionRepo causes Delete on reactionRepo to fail (for handleUndoLike).
type deleteFailReactionRepo struct {
	*testutil.MockNoteReactionRepository
}

func (r *deleteFailReactionRepo) Delete(_ *model.NoteReaction) (int64, error) {
	return 0, errors.New("react delete failed")
}

// deleteFailFollowingRepo causes Delete on followingRepo to fail (for handleReject Unfollow).
type deleteFailFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (r *deleteFailFollowingRepo) Delete(_ *model.Following) error {
	return errors.New("following delete failed")
}

// deleteFailFollowRequestRepo causes Delete on followRequestRepo to fail (for handleReject CancelRequest).
type deleteFailFollowRequestRepo struct {
	*testutil.MockFollowRequestRepository
}

func (r *deleteFailFollowRequestRepo) Delete(_ *model.FollowRequest) error {
	return errors.New("request delete failed")
}

func TestProcess_UndoUnknownInnerType(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type": "Move"}
	}`)
	err := p.Process(undo)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

// extractLocalNoteID with urls==nil branch.
func TestResolveNote_NilURLs(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", URI: &uri}
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, nil, &stubFetcher{}, idGen)
	got, err := r.ResolveNote(uri)
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

// handleUndoLike: reactionService.Delete returns a non-NotFound error.
func TestProcess_UndoLikeDeleteError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	mockReactionRepo := testutil.NewMockNoteReactionRepository()
	// 既存リアクションを 1 つ仕込んでおく (FindByPair が成功するように)
	mockReactionRepo.Reactions["r1"] = &model.NoteReaction{ID: "r1", UserID: "alice-id", NoteID: "n1", Reaction: "👍"}
	reactionRepo := &deleteFailReactionRepo{MockNoteReactionRepository: mockReactionRepo}
	emojiRepo := testutil.NewMockEmojiRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// alice を予め repo に置いておく (resolver が新規作成しないように)
	aliceURI := "https://remote.example/users/alice"
	userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}
	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	reactionSvc := corereaction.NewService(noteRepo, reactionRepo, emojiRepo, followingRepo, idGen)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	p := federation.NewProcessor(resolver, followingSvc, reactionSvc, deleteSvc, userRepo, noteRepo)

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Like","object":"https://example.com/notes/n1"}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

// handleUndoAnnounce: ListRenotesOf returns error.
func TestProcess_UndoAnnounceListError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	mockNoteRepo := testutil.NewMockNoteRepository()
	mockNoteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	noteRepo := &listRenotesFailRepo{MockNoteRepository: mockNoteRepo}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(userRepo, testutil.NewMockFollowingRepository(), testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, userRepo, noteRepo)

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Announce","object":"https://example.com/notes/n1"}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

// handleUndoAnnounce: skip renote owned by another user / skip non-pure renote / Delete error.
func TestProcess_UndoAnnounceSkipsAndDeleteError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	mockNoteRepo := testutil.NewMockNoteRepository()
	target := &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	mockNoteRepo.Notes["n1"] = target
	// alice (announcer) を予め登録 → resolver は新規作成しない
	aliceURI := "https://remote.example/users/alice"
	userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}
	// renote 1: 別ユーザー (skip 対象)
	mockNoteRepo.Notes["r-other"] = &model.Note{ID: "r-other", UserID: "other", RenoteID: &target.ID}
	// renote 2: alice の quote renote (text あり: pure renote ではない)
	text := "quote!"
	mockNoteRepo.Notes["r-quote"] = &model.Note{ID: "r-quote", UserID: "alice-id", RenoteID: &target.ID, Text: &text}
	// renote 3: alice の pure renote → これが削除対象だが Delete エラーで失敗
	mockNoteRepo.Notes["r-pure"] = &model.Note{ID: "r-pure", UserID: "alice-id", RenoteID: &target.ID}

	noteRepo := &deleteFailNoteRepo{MockNoteRepository: mockNoteRepo}

	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(userRepo, testutil.NewMockFollowingRepository(), testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, userRepo, noteRepo)

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Announce","object":"https://example.com/notes/n1"}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

// handleUndoAnnounce: only non-matching renotes exist → loop completes without
// deleting anything but exercises both continue branches.
func TestProcess_UndoAnnounceNoMatchSkips(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	target := &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	env.noteRepo.Notes["n1"] = target
	// alice (announcer) を予め登録
	aliceURI := "https://remote.example/users/alice"
	env.userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}
	// renote by other user (skip 対象)
	env.noteRepo.Notes["r-other"] = &model.Note{ID: "r-other", UserID: "other", RenoteID: &target.ID}
	// alice の quote renote (pure ではない → skip)
	text := "quote!"
	env.noteRepo.Notes["r-quote"] = &model.Note{ID: "r-quote", UserID: "alice-id", RenoteID: &target.ID, Text: &text}

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Announce","object":"https://example.com/notes/n1"}
	}`)
	require.NoError(t, env.processor.Process(undo))
}

// TestProcess_LikeTransientFetchError: 5xx 等の transient な fetcher error は
// 従来通り err 返却で activity を retry サイクルに乗せる (#1183 の best-effort
// 化は permanent error 限定であって、transient はそのまま retry させる)。
func TestProcess_LikeTransientFetchError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	reactionRepo := testutil.NewMockNoteReactionRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// 502 Bad Gateway = transient 障害想定。permanent error の whitelist
	// (401/403/404/410 / ErrInvalidActor / ErrInvalidNote / ErrNoteNotVisible)
	// に該当しないため、handler は err を caller に伝搬 = failed bucket → retry。
	fetcher := &stubFetcher{err: &activitypub.StatusError{StatusCode: 502, Status: "502 Bad Gateway"}}
	resolver := federation.NewResolver(userRepo, noteRepo, urls, fetcher, idGen)
	resolver.SetEmojiRepo(emojiRepo)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	reactionSvc := corereaction.NewService(noteRepo, reactionRepo, emojiRepo, followingRepo, idGen)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	p := federation.NewProcessor(resolver, followingSvc, reactionSvc, deleteSvc, userRepo, noteRepo)

	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	err := p.Process(body)
	require.Error(t, err, "transient (5xx) は retry されるべき")
	assert.Empty(t, reactionRepo.Reactions)
}

// handleLike: reactionService.Create returns ErrNoteNotVisible (followers-only
// target).best-effort ack (#1183): visibility 違反は permanent fail として
// ack し、reaction record を作らない。
func TestProcess_LikeNotVisible(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     "bob",
		Visibility: model.NoteVisibilityFollowers,
	}
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "👍"
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Empty(t, env.reactionRepo.Reactions, "visibility 違反は reaction を作らない")
}

// handleReject: Unfollow returns a non-NotFollowing error.
func TestProcess_RejectUnfollowError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	mockFR := testutil.NewMockFollowingRepository()
	// pre-existing follow record so Unfollow finds it
	mockFR.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice-id"}
	followingRepo := &deleteFailFollowingRepo{MockFollowingRepository: mockFR}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	bobURI := "https://example.com/users/bob"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	aliceURI := "https://remote.example/users/alice"
	userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}

	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, userRepo, noteRepo)

	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/bob",
			"object": "https://remote.example/users/alice"
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
}

// handleReject: CancelRequest returns a non-NotRequest error.
func TestProcess_RejectCancelError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	mockFR := testutil.NewMockFollowingRepository()
	mockFRR := testutil.NewMockFollowRequestRepository()
	// pending request の存在: FindByPair が成功するように仕込む
	mockFRR.Requests["fr1"] = &model.FollowRequest{ID: "fr1", FollowerID: "bob", FolloweeID: "alice-id"}
	frRepo := &deleteFailFollowRequestRepo{MockFollowRequestRepository: mockFRR}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	bobURI := "https://example.com/users/bob"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	aliceURI := "https://remote.example/users/alice"
	userRepo.Users["alice-id"] = &model.User{ID: "alice-id", Username: "alice", URI: &aliceURI}

	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(userRepo, mockFR, frRepo, idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, userRepo, noteRepo)

	body := []byte(`{
		"type": "Reject",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/bob",
			"object": "https://remote.example/users/alice"
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
}

// --- JSON-LD normalization (Step I) ------------------------------------------

// Process accepts activities whose keys use the "as:" prefix or full IRI form.
// activitypub.Normalize はそれらを canonical な短形式に変換する。
func TestProcess_NormalizesPrefixedFollow(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"as:type": "Follow",
		"as:actor": "https://remote.example/users/alice",
		"as:object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, followingRepo.Followings, 1)
}

func TestProcess_NormalizesIRIFollow(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"https://www.w3.org/ns/activitystreams#type": "Follow",
		"https://www.w3.org/ns/activitystreams#actor": "https://remote.example/users/alice",
		"https://www.w3.org/ns/activitystreams#object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, followingRepo.Followings, 1)
}

func TestProcess_NormalizesTypeArray(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": ["Follow", "https://example.com/SomethingElse"],
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, followingRepo.Followings, 1)
}

func TestProcess_NormalizesObjectIDShortcut(t *testing.T) {
	// {"object": {"@id": "..."}} 形式は string に縮約される
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": {"@id": "https://example.com/users/bob"}
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, followingRepo.Followings, 1)
}

func TestProcess_AnnounceCreateError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := &noteCreateFailRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	emojiRepo := testutil.NewMockEmojiRepository()
	reactionRepo := testutil.NewMockNoteReactionRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(userRepo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	reactionSvc := corereaction.NewService(noteRepo, reactionRepo, emojiRepo, followingRepo, idGen)
	deleteSvc := corenote.NewDeleteService(noteRepo)
	p := federation.NewProcessor(resolver, followingSvc, reactionSvc, deleteSvc, userRepo, noteRepo)

	body := []byte(`{
		"type": "Announce",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)
	err := p.Process(body)
	assert.Error(t, err)
}

// #2023: OrderedCollection / Collection を受信したら items を unroll し、id host が
// collection actor の host と一致する各 item を dispatch する。host 不一致 item は skip。
func TestProcess_CollectionUnrollsItems(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "OrderedCollection",
		"actor": "https://remote.example/users/alice",
		"orderedItems": [
			{
				"id": "https://remote.example/likes/l1",
				"type": "Like",
				"actor": "https://remote.example/users/alice",
				"object": "https://example.com/notes/n1",
				"content": "⭐"
			}
		]
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1, "collection 内の Like が dispatch される (#2023)")
}

// #2023: item の id host が collection actor の host と一致しないときは skip する。
func TestProcess_CollectionSkipsHostMismatch(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Collection",
		"actor": "https://remote.example/users/alice",
		"items": [
			{
				"id": "https://evil.example/likes/l2",
				"type": "Like",
				"actor": "https://remote.example/users/alice",
				"object": "https://example.com/notes/n1",
				"content": "❤"
			}
		]
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Empty(t, env.reactionRepo.Reactions, "host 不一致 item は skip (#2023)")
}

// item に型エラーがあっても丸ごと落とさない (#2662)。`actor` が object 形式でも
// item.actor は使わない (collection actor の権威で処理する) ので、認可は変わらない。
// id host gate はそのまま効く。
func TestProcess_CollectionItemTypeErrorStillDispatched(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Collection",
		"actor": "https://remote.example/users/alice",
		"items": [
			{
				"id": "https://remote.example/likes/l1",
				"type": "Like",
				"actor": {"id": "https://remote.example/users/alice"},
				"object": "https://example.com/notes/n1",
				"content": "⭐"
			}
		]
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1, "型エラーのある item を落とさない")
}

// `items` / `orderedItems` が単一 object の Collection。`[]json.RawMessage`
// 決め打ちだとこの形の item が 1 件も dispatch されない (#2665)。
//
// **両方を見る。** AS2 の normative context では `items` が
// `{"@type": "@id"}`、`orderedItems` が `@container: @list` なので、厳密な
// compaction で潰れるのは `items` 側だけ。ただし mk-go の
// `activitypub.Normalize` は本物の compaction ではなく、手書き JSON を出す
// peer のスカラー `orderedItems` はそのまま到達する。upstream も
// `toArray(activity.orderedItems)` で受ける (ApInboxService.ts:100)。
func TestProcess_CollectionSingleObjectItems(t *testing.T) {
	for _, tc := range []struct {
		name     string
		colType  string
		itemsKey string
	}{
		{"Collection items", "Collection", "items"},
		{"OrderedCollection orderedItems", "OrderedCollection", "orderedItems"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newFullProcessor(t, aliceActor)
			env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
			body := []byte(`{
				"type": "` + tc.colType + `",
				"actor": "https://remote.example/users/alice",
				"` + tc.itemsKey + `": {
					"id": "https://remote.example/likes/l1",
					"type": "Like",
					"actor": "https://remote.example/users/alice",
					"object": "https://example.com/notes/n1",
					"content": "⭐"
				}
			}`)
			require.NoError(t, env.processor.Process(body))
			require.Len(t, env.reactionRepo.Reactions, 1, "単一 object を 1 件として dispatch する")
		})
	}
}

// 文字列 URI だけの item は従来どおり skip する (id が無いので host gate に
// 掛からず、そもそも dispatch できない)。
func TestProcess_CollectionSkipsStringItem(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	body := []byte(`{
		"type": "Collection",
		"actor": "https://remote.example/users/alice",
		"items": ["https://remote.example/likes/l1"]
	}`)
	require.NoError(t, env.processor.Process(body))
	assert.Empty(t, env.reactionRepo.Reactions, "文字列 item は skip")
}

// #2023 (security): collection item が別 actor を詐称しても、collection の認証 actor の
// 権威で処理される (item.actor は使わない)。詐称 reaction が victim 名義で記録されない。
func TestProcess_CollectionItemActorSpoofIgnored(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	// collection actor は alice@remote.example。item は victim@other.example を actor に詐称。
	// item.id host は remote.example で collection actor と一致するため host check は通過するが、
	// actor は collection actor (alice) に上書きされる。
	body := []byte(`{
		"type": "OrderedCollection",
		"actor": "https://remote.example/users/alice",
		"orderedItems": [
			{
				"id": "https://remote.example/likes/spoof",
				"type": "Like",
				"actor": "https://other.example/users/victim",
				"object": "https://example.com/notes/n1",
				"content": "⭐"
			}
		]
	}`)
	require.NoError(t, env.processor.Process(body))
	require.Len(t, env.reactionRepo.Reactions, 1)
	for _, r := range env.reactionRepo.Reactions {
		// reaction は alice (collection actor) 名義であり victim 名義ではない。
		assert.NotContains(t, r.UserID, "victim", "詐称 actor では記録されない (#2023 security)")
	}
}

// findFailUserRepo makes the author lookup fail, so the attribution check
// cannot confirm who owns the note.
type findFailUserRepo struct {
	*testutil.MockUserRepository
	failFor  string
	withUser bool
}

func (r *findFailUserRepo) FindByID(id string) (*model.User, error) {
	if id == r.failFor {
		if r.withUser {
			// **値と error を同時に返す形も見る。** `author == nil` だけを
			// 見る実装だとここが素通りし、`aerr != nil` の判定が空虚になる。
			u, _ := r.MockUserRepository.FindByID(id)
			return u, errors.New("user SELECT failed")
		}
		return nil, errors.New("user SELECT failed")
	}
	return r.MockUserRepository.FindByID(id)
}

// **著者を確認できないなら更新しない (#3037)。**
//
// 以前は「`FindByID` が成功し、`author.URI` が非 nil で、値が違うとき」だけ
// 拒否していたので、**DB 障害 / 行の消失 / URI が NULL のどれでも検査が
// 丸ごと消えて**いた。ここは「他人のノートを書き換えられるか」を決める
// 唯一の検査で、通れば `text` / `cw` / `url` に加えて `upsertEmojis` が
// 使う host まで攻撃者の言い値になる。
func TestProcess_UpdateNote_AuthorLookupFailureDoesNotUpdate(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*testing.T, *testutil.MockUserRepository) repository.UserRepository
	}{
		{
			name: "著者の lookup が失敗する",
			setup: func(_ *testing.T, base *testutil.MockUserRepository) repository.UserRepository {
				return &findFailUserRepo{MockUserRepository: base, failFor: "alice_remote"}
			},
		},
		{
			name: "lookup が値と error を同時に返す",
			setup: func(_ *testing.T, base *testutil.MockUserRepository) repository.UserRepository {
				return &findFailUserRepo{MockUserRepository: base, failFor: "alice_remote", withUser: true}
			},
		},
		{
			name: "著者の行が消えている",
			setup: func(_ *testing.T, base *testutil.MockUserRepository) repository.UserRepository {
				delete(base.Users, "alice_remote")
				return base
			},
		},
		{
			name: "著者の URI が NULL",
			setup: func(_ *testing.T, base *testutil.MockUserRepository) repository.UserRepository {
				base.Users["alice_remote"].URI = nil
				return base
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			host := "remote.example"
			aliceURI := "https://remote.example/users/alice"
			userRepo.Users["alice_remote"] = &model.User{
				ID: "alice_remote", Username: "alice", Host: &host, URI: &aliceURI,
			}
			uri := "https://remote.example/notes/n1"
			original := "original"
			noteRepo.Notes["n1"] = &model.Note{
				ID: "n1", URI: &uri, UserID: "alice_remote", UserHost: &host, Text: &original,
			}

			effective := tt.setup(t, userRepo)
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			resolver := federation.NewResolver(effective, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
			followingSvc := corefollowing.NewService(effective, testutil.NewMockFollowingRepository(), testutil.NewMockFollowRequestRepository(), idGen)
			p := federation.NewProcessor(resolver, followingSvc, nil, nil, effective, noteRepo)

			body := []byte(`{
				"type": "Update",
				"actor": "https://remote.example/users/alice",
				"object": {
					"id": "https://remote.example/notes/n1",
					"type": "Note",
					"attributedTo": "https://remote.example/users/alice",
					"content": "edited"
				}
			}`)
			_ = p.Process(body)

			got := noteRepo.Notes["n1"]
			require.NotNil(t, got.Text)
			assert.Equal(t, "original", *got.Text, "著者を確認できないのに更新している")
		})
	}
}

// **poll 側も fail-closed であること (#3037 レビュー)。**
//
// このコミットは Note と Question の**両方**を fail-closed にしたが、テストが
// 増えたのは Note 側だけだった。Question の guard を旧 fail-open 形に戻しても
// 誰も気付かない状態 (レビュアーが変異検証で実測)。
func TestProcess_UpdateQuestion_AuthorLookupFailureDoesNotUpdate(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*testing.T, *testutil.MockUserRepository) repository.UserRepository
		// wantErr は「判定できなかった」ぶんを job へ返すか (#3116)。
		// **更新しないことと ack することは別**で、not-found (行が消えた /
		// URI が NULL) は retry しても変わらないので ack、DB 障害は伝播させる。
		wantErr bool
	}{
		{
			name: "著者の lookup が失敗する",
			setup: func(_ *testing.T, base *testutil.MockUserRepository) repository.UserRepository {
				return &findFailUserRepo{MockUserRepository: base, failFor: "alice_remote"}
			},
			wantErr: true,
		},
		{
			name: "著者の行が消えている",
			setup: func(_ *testing.T, base *testutil.MockUserRepository) repository.UserRepository {
				delete(base.Users, "alice_remote")
				return base
			},
		},
		{
			name: "著者の URI が NULL",
			setup: func(_ *testing.T, base *testutil.MockUserRepository) repository.UserRepository {
				base.Users["alice_remote"].URI = nil
				return base
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			pollRepo := testutil.NewMockPollRepository()
			host := "remote.example"
			aliceURI := "https://remote.example/users/alice"
			userRepo.Users["alice_remote"] = &model.User{
				ID: "alice_remote", Username: "alice", Host: &host, URI: &aliceURI,
			}
			uri := "https://remote.example/notes/q1"
			noteRepo.Notes["q1"] = &model.Note{ID: "q1", URI: &uri, UserID: "alice_remote", UserHost: &host}
			pollRepo.Polls["q1"] = &model.Poll{
				NoteID: "q1", Choices: []string{"a", "b"}, Votes: pgarray.Int64Array{0, 0},
			}

			effective := tt.setup(t, userRepo)
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			resolver := federation.NewResolver(effective, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
			resolver.SetPollRepo(pollRepo)

			err := resolver.UpdateRemoteQuestion(json.RawMessage(`{
				"id": "https://remote.example/notes/q1",
				"type": "Question",
				"attributedTo": "https://remote.example/users/alice",
				"oneOf": [
					{"name": "a", "replies": {"totalItems": 99}},
					{"name": "b", "replies": {"totalItems": 99}}
				]
			}`), "https://remote.example/users/alice")
			if tt.wantErr {
				require.Error(t, err, "DB 障害を ack している (job が retry されない)")
			} else {
				require.NoError(t, err, "not-found を retry に倒している")
			}

			assert.Equal(t, pgarray.Int64Array{0, 0}, pollRepo.Polls["q1"].Votes,
				"著者を確認できないのに票数を更新している")
		})
	}
}

// #3201: リモートからのリノート取り消し (Undo(Announce)) でも通知を消す。
// この経路は DeleteService を通らないので、hook を直接呼ばないと丸ごと漏れる。
func TestProcess_UndoAnnounceRemovesRenoteNotification(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	env.noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
	hook := &fakeNotificationHook{}
	env.processor.SetNotificationHook(hook)

	require.NoError(t, env.processor.Process([]byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1"
	}`)))
	var boost *model.Note
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == "n1" {
			boost = n
		}
	}
	require.NotNil(t, boost)
	require.NoError(t, env.processor.Process([]byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type": "Announce", "object": "https://example.com/notes/n1"}
	}`)))

	hook.mu.Lock()
	defer hook.mu.Unlock()
	// 通知は (受信者 = リノート先の作者, 通知者 = リノートした人, NoteID = リノート自身) で
	// 引くので、消したブーストそのものが 3 つとも持って渡ること。
	assert.Equal(t, []string{boost.ID + " " + boost.UserID + "->bob"}, hook.deleted)
}
