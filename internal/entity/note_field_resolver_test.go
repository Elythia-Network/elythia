package entity

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubDriveFileLookup / stubChannelLookup / stubNoteReactionLookup の最小実装。
// 個別 repository から repository 同士の依存を持ち込まないよう entity package
// 内で完結させる。

type stubDriveFileLookup struct {
	files map[string]*model.DriveFile
	// calls は FindByIDs の呼び出し回数。バッチ化 (ページ全体で 1 回) と、
	// 対象が無いときに引かないことを固定するテストが読む。
	calls int
}

func (s *stubDriveFileLookup) FindByIDs(ids []string) ([]*model.DriveFile, error) {
	s.calls++
	out := make([]*model.DriveFile, 0, len(ids))
	for _, id := range ids {
		if f, ok := s.files[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

type stubNoteReactionLookup struct {
	rows map[string]*model.NoteReaction // noteID -> reaction (per-viewer は呼び出し側で識別)
}

func (s *stubNoteReactionLookup) FindByUserAndNoteIDs(_ string, noteIDs []string) (map[string]*model.NoteReaction, error) {
	out := make(map[string]*model.NoteReaction, len(noteIDs))
	for _, nid := range noteIDs {
		if r, ok := s.rows[nid]; ok {
			out[nid] = r
		}
	}
	return out, nil
}

type stubChannelLookup struct {
	channels map[string]*model.Channel
}

func (s *stubChannelLookup) FindByIDs(ids []string) ([]*model.Channel, error) {
	out := make([]*model.Channel, 0, len(ids))
	for _, id := range ids {
		if ch, ok := s.channels[id]; ok {
			out = append(out, ch)
		}
	}
	return out, nil
}

func makeIDGen(t *testing.T) id.Generator {
	t.Helper()
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return g
}

// Renote / Reply の embed にも Files が解決されることを確認する (#426 の
// 中心ケース)。
func TestNoteFieldResolver_ResolveFiles_EmbedsRenoteAndReply(t *testing.T) {
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", Name: "outer.png", Type: "image/png", URL: "https://example.com/f1"},
		"f2": {ID: "f2", Name: "renote.png", Type: "image/png", URL: "https://example.com/f2"},
		"f3": {ID: "f3", Name: "reply.png", Type: "image/png", URL: "https://example.com/f3"},
	}}
	r := NewNoteFieldResolver(files, nil, nil, nil, nil, makeIDGen(t))

	notes := []NoteEntity{{
		ID:      "n1",
		FileIDs: model.StringArray{"f1"},
		Renote:  &NoteEntity{ID: "n2", FileIDs: model.StringArray{"f2"}},
		Reply:   &NoteEntity{ID: "n3", FileIDs: model.StringArray{"f3"}},
	}}
	r.ResolveFiles(notes)

	require.Len(t, notes[0].Files, 1)
	require.Len(t, notes[0].Renote.Files, 1)
	require.Len(t, notes[0].Reply.Files, 1)
}

// renote embed には MyReaction / Channel が解決され、reply embed は detail:false
// なので MyReaction を持たない (Channel は detail 外なので reply にも付く、#1816)。
func TestNoteFieldResolver_ResolveViewerFields_EmbedsRenoteAndReply(t *testing.T) {
	reactions := &stubNoteReactionLookup{rows: map[string]*model.NoteReaction{
		"n1": {NoteID: "n1", Reaction: "👍"},
		"n2": {NoteID: "n2", Reaction: "❤"},
		"n3": {NoteID: "n3", Reaction: "🎉"},
	}}
	chID := "ch1"
	channels := &stubChannelLookup{channels: map[string]*model.Channel{
		chID: {ID: chID, Name: "general"},
	}}
	r := NewNoteFieldResolver(nil, nil, nil, reactions, channels, makeIDGen(t))

	// #1641: myReaction は reactionCount>0 の note だけ fetch するので、
	// reaction を持つ note には ReactionCount を立てる (reaction row があるのに
	// count=0 は本来あり得ない状態)。
	notes := []NoteEntity{{
		ID:            "n1",
		ChannelID:     &chID,
		ReactionCount: 1,
		Renote:        &NoteEntity{ID: "n2", ChannelID: &chID, ReactionCount: 1},
		Reply:         &NoteEntity{ID: "n3", ReactionCount: 1},
	}}
	r.ResolveViewerFields(notes, &model.User{ID: "v1"})

	require.NotNil(t, notes[0].MyReaction)
	require.NotNil(t, notes[0].Renote.MyReaction)
	// reply embed (detail:false) は myReaction を持たない (#1816)。
	require.Nil(t, notes[0].Reply.MyReaction)
	require.NotNil(t, notes[0].Channel)
	assert.Equal(t, "general", notes[0].Channel.Name)
	require.NotNil(t, notes[0].Renote.Channel)
}

// #1816: myReaction も legacy text alias (like→👍) を Unicode へ変換する
// (upstream populateMyReaction、reactions map と一貫)。
func TestNoteFieldResolver_MyReactionLegacyConverted(t *testing.T) {
	reactions := &stubNoteReactionLookup{rows: map[string]*model.NoteReaction{
		"n1": {NoteID: "n1", Reaction: "like"},
	}}
	r := NewNoteFieldResolver(nil, nil, nil, reactions, nil, makeIDGen(t))

	notes := []NoteEntity{{ID: "n1", ReactionCount: 1}}
	r.ResolveViewerFields(notes, &model.User{ID: "v1"})

	require.NotNil(t, notes[0].MyReaction)
	assert.Equal(t, "👍", *notes[0].MyReaction, "legacy text reaction は myReaction で Unicode に変換")
}

// nil receiver / nil viewer / 空 slice / nil lookup の網羅的 nil safe 確認。
func TestNoteFieldResolver_NilSafe(t *testing.T) {
	var r *NoteFieldResolver
	r.Apply(nil, nil) // nil receiver
	r.ResolveFiles(nil)
	r.ResolveViewerFields(nil, nil)

	r2 := NewNoteFieldResolver(nil, nil, nil, nil, nil, nil)
	r2.Apply([]NoteEntity{{ID: "n1"}}, nil)

	// 個別 helper も nil 入力で panic しない
	applyMyReaction(nil, nil)
	applyChannel(nil, nil)
	assert.Equal(t, []string(nil), appendNoteIDs(nil, nil))
	assert.Equal(t, []string(nil), appendNoteFileIDs(nil, nil))
	assert.Equal(t, []string(nil), appendNoteChannelIDs(nil, nil))
}

type stubFolderLookup struct {
	folders map[string]*model.DriveFolder
	err     error
}

func (s *stubFolderLookup) FindByID(id string) (*model.DriveFolder, error) {
	if s.err != nil {
		return nil, s.err
	}
	if f, ok := s.folders[id]; ok {
		return f, nil
	}
	return nil, assertError("not found")
}

type stubFileOwnerLookup struct {
	users map[string]*model.User
	err   error
}

func (s *stubFileOwnerLookup) FindByID(id string) (*model.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return nil, assertError("not found")
}

// helper: build error with message (avoid repeated `errors.New`).
type assertError string

func (e assertError) Error() string { return string(e) }

// 添付ファイルの folder / owner も embed されることを確認 (#317 経路の
// resolver 統合)。folder lookup ヒット + owner lookup ヒット + 失敗 fallback
// を 1 ノートで cover する。
func TestNoteFieldResolver_ResolveFiles_EmbedsFolderAndOwner(t *testing.T) {
	folderID := "folder-1"
	owner := "user-1"
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", FolderID: &folderID, UserID: &owner, Name: "x.png", Type: "image/png", URL: "https://example.com/f1"},
		"f2": {ID: "f2", Name: "lone.png", Type: "image/png", URL: "https://example.com/f2"}, // folderID / userID 共に nil
	}}
	folders := &stubFolderLookup{folders: map[string]*model.DriveFolder{
		folderID: {ID: folderID, Name: "media"},
	}}
	owners := &stubFileOwnerLookup{users: map[string]*model.User{
		owner: {ID: owner, Username: "alice"},
	}}
	r := NewNoteFieldResolver(files, folders, owners, nil, nil, makeIDGen(t))

	notes := []NoteEntity{{ID: "n1", FileIDs: model.StringArray{"f1", "f2"}}}
	r.ResolveFiles(notes)
	require.Len(t, notes[0].Files, 2)
}

// batchFolderStub / batchOwnerStub は optional batch interface
// (FindByIDs / FindManyByIDs) を実装し、呼び出し回数と渡された ID を記録する。
type batchFolderStub struct {
	folders map[string]*model.DriveFolder
	calls   int
	lastIDs []string
}

func (s *batchFolderStub) FindByID(id string) (*model.DriveFolder, error) {
	if f, ok := s.folders[id]; ok {
		return f, nil
	}
	return nil, assertError("not found")
}

func (s *batchFolderStub) FindByIDs(ids []string) ([]*model.DriveFolder, error) {
	s.calls++
	s.lastIDs = ids
	out := make([]*model.DriveFolder, 0, len(ids))
	for _, id := range ids {
		if f, ok := s.folders[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

type batchOwnerStub struct {
	users map[string]*model.User
	calls int
}

func (s *batchOwnerStub) FindByID(id string) (*model.User, error) {
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return nil, assertError("not found")
}

func (s *batchOwnerStub) FindManyByIDs(ids []string) ([]*model.User, error) {
	s.calls++
	out := make([]*model.User, 0, len(ids))
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// lookup が batch interface を実装する場合、distinct folder / owner を 1 query
// ずつで引く (= per-distinct-ID の FindByID を畳む N+1 解消、#1389)。
func TestNoteFieldResolver_ResolveFiles_BatchesFolderAndOwner(t *testing.T) {
	fA, fB := "folder-a", "folder-b"
	uA, uB := "user-a", "user-b"
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", FolderID: &fA, UserID: &uA, Name: "1.png", Type: "image/png", URL: "https://e/1"},
		"f2": {ID: "f2", FolderID: &fB, UserID: &uB, Name: "2.png", Type: "image/png", URL: "https://e/2"},
		// f3 は f1 と同じ folder / owner → dedup されて batch ID には重複しない。
		"f3": {ID: "f3", FolderID: &fA, UserID: &uA, Name: "3.png", Type: "image/png", URL: "https://e/3"},
	}}
	folders := &batchFolderStub{folders: map[string]*model.DriveFolder{
		fA: {ID: fA, Name: "A"}, fB: {ID: fB, Name: "B"},
	}}
	owners := &batchOwnerStub{users: map[string]*model.User{
		uA: {ID: uA, Username: "a"}, uB: {ID: uB, Username: "b"},
	}}
	r := NewNoteFieldResolver(files, folders, owners, nil, nil, makeIDGen(t))

	notes := []NoteEntity{{ID: "n1", FileIDs: model.StringArray{"f1", "f2", "f3"}}}
	r.ResolveFiles(notes)

	require.Len(t, notes[0].Files, 3)
	// distinct folder / owner が 2 件あっても batch なら 1 query ずつ。
	assert.Equal(t, 1, folders.calls, "FindByIDs は 1 回 (per-distinct-ID でない)")
	assert.Equal(t, 1, owners.calls, "FindManyByIDs は 1 回")
	// dedup 済み distinct ID が渡る (f1/f3 が共有する fA は 1 度だけ)。
	assert.ElementsMatch(t, []string{fA, fB}, folders.lastIDs)
}

// folder / owner lookup が err を返してもパニックせず folder/user nil で
// pack される。
func TestNoteFieldResolver_ResolveFiles_LookupErrorsTolerated(t *testing.T) {
	folderID := "folder-x"
	owner := "user-x"
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", FolderID: &folderID, UserID: &owner, Name: "x.png", Type: "image/png", URL: "https://example.com/f1"},
	}}
	folders := &stubFolderLookup{err: assertError("db down")}
	owners := &stubFileOwnerLookup{err: assertError("db down")}
	r := NewNoteFieldResolver(files, folders, owners, nil, nil, makeIDGen(t))

	notes := []NoteEntity{{ID: "n1", FileIDs: model.StringArray{"f1"}}}
	r.ResolveFiles(notes)
	require.Len(t, notes[0].Files, 1)
}

// viewer == nil の時は MyReaction を埋めない (Channel は埋める)。
func TestNoteFieldResolver_NilViewerSkipsMyReaction(t *testing.T) {
	reactions := &stubNoteReactionLookup{rows: map[string]*model.NoteReaction{
		"n1": {NoteID: "n1", Reaction: "👍"},
	}}
	chID := "ch1"
	channels := &stubChannelLookup{channels: map[string]*model.Channel{
		chID: {ID: chID, Name: "general"},
	}}
	r := NewNoteFieldResolver(nil, nil, nil, reactions, channels, makeIDGen(t))

	notes := []NoteEntity{{ID: "n1", ChannelID: &chID}}
	r.ResolveViewerFields(notes, nil)
	assert.Nil(t, notes[0].MyReaction)
	assert.NotNil(t, notes[0].Channel)
}

// SetPollVoteLookup の wire を確認する小さな regression test (#710 で
// entity 全体カバレッジを 90% 上に保つために追加)。nil receiver で no-op
// 化し、non-nil receiver では fields に設定されることを確認する。
func TestNoteFieldResolver_SetPollVoteLookup(t *testing.T) {
	// nil receiver は panic せず黙って return する。
	var nilResolver *NoteFieldResolver
	nilResolver.SetPollVoteLookup(nil)

	// non-nil receiver は内部 field を設定する。SetPollVoteLookup は
	// public な observation 経路を持たないので、設定後に再度呼んで
	// 上書きが効くこと (idempotent) のみ確認する。
	r := &NoteFieldResolver{}
	r.SetPollVoteLookup(nil)
	r.SetPollVoteLookup(nil)
}

// stubPollVoteLookup is a fake PollVoteLookup that returns a pre-configured
// noteID → [choice indices] map. Used to drive the appendPollNoteIDs /
// applyMyPollVotes / applyVotedChoices coverage gap (#739)。
type stubPollVoteLookup struct {
	votes map[string][]int
}

func (s *stubPollVoteLookup) FindByUserAndNoteIDs(_ string, _ []string) (map[string][]int, error) {
	return s.votes, nil
}

// #739: ResolveViewerFields 経由で appendPollNoteIDs / applyMyPollVotes /
// applyVotedChoices を全部踏ませる。top-level / Renote / Reply の 3 経路を
// すべて含む note slice を組み立てる。
func TestNoteFieldResolver_ResolveViewerFields_PollVoted(t *testing.T) {
	mkPoll := func() *PollEntity {
		return &PollEntity{Choices: []PollChoice{{Text: "a"}, {Text: "b"}, {Text: "c"}}}
	}
	notes := []NoteEntity{
		{
			ID:   "top",
			Poll: mkPoll(),
			Renote: &NoteEntity{
				ID:   "renoteTarget",
				Poll: mkPoll(),
			},
			Reply: &NoteEntity{
				ID:   "replyTarget",
				Poll: mkPoll(),
			},
		},
	}

	r := &NoteFieldResolver{
		pollVote: &stubPollVoteLookup{
			votes: map[string][]int{
				"top":          {0},
				"renoteTarget": {1},
				"replyTarget":  {2},
				// 範囲外 index も混ぜて applyVotedChoices の bounds check
				// (idx >= 0 && idx < len) を踏ませる
			},
		},
	}
	viewer := &model.User{ID: "v1"}
	r.ResolveViewerFields(notes, viewer)

	assert.True(t, notes[0].Poll.Choices[0].IsVoted, "top: choice 0")
	assert.True(t, notes[0].Renote.Poll.Choices[1].IsVoted, "renote: choice 1")
	assert.True(t, notes[0].Reply.Poll.Choices[2].IsVoted, "reply: choice 2")
}

// applyVotedChoices の bounds check (idx out of range) と空 choices guard
// 経路を踏む。
func TestNoteFieldResolver_ResolveViewerFields_PollVoted_OutOfRange(t *testing.T) {
	notes := []NoteEntity{
		{
			ID: "top",
			Poll: &PollEntity{
				Choices: []PollChoice{{Text: "a"}},
			},
		},
	}
	r := &NoteFieldResolver{
		pollVote: &stubPollVoteLookup{
			votes: map[string][]int{"top": {99, -1}}, // 範囲外なので無視される
		},
	}
	r.ResolveViewerFields(notes, &model.User{ID: "v1"})
	// 範囲外のため何もマークされない
	assert.False(t, notes[0].Poll.Choices[0].IsVoted)
}

// #739: packPoll の主要分岐を unit test。nil / choices/votes mismatch /
// expiresAt 非 nil をすべて踏む。
func TestPackPoll(t *testing.T) {
	// nil → nil
	assert.Nil(t, packPoll(nil))

	// choices < votes (votes 余り無視)
	p := &model.Poll{
		Choices: []string{"a", "b"},
		Votes:   model.Int64Array{3, 7, 99}, // 99 は破棄される
	}
	got := packPoll(p)
	require.NotNil(t, got)
	require.Len(t, got.Choices, 2)
	assert.Equal(t, "a", got.Choices[0].Text)
	assert.Equal(t, 3, got.Choices[0].Votes)
	assert.Equal(t, 7, got.Choices[1].Votes)
	assert.Nil(t, got.ExpiresAt)

	// expiresAt 非 nil → ISO8601 ms 文字列
	t1 := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	p2 := &model.Poll{
		Choices:   []string{"x"},
		Votes:     model.Int64Array{0},
		ExpiresAt: &t1,
	}
	got2 := packPoll(p2)
	require.NotNil(t, got2.ExpiresAt)
	assert.Equal(t, "2026-05-06T12:00:00.000Z", *got2.ExpiresAt)

	// votes < choices (choices 多い → votes=0 で埋まる)
	p3 := &model.Poll{
		Choices: []string{"a", "b", "c"},
		Votes:   model.Int64Array{1},
	}
	got3 := packPoll(p3)
	assert.Equal(t, 1, got3.Choices[0].Votes)
	assert.Equal(t, 0, got3.Choices[1].Votes)
	assert.Equal(t, 0, got3.Choices[2].Votes)
}

// #1641: reactionCount==0 の note は myReaction fetch 対象から外す
// (upstream populateMyReaction の reactionsCount===0 skip 相当)。
type countingReactionLookup struct {
	rows      map[string]*model.NoteReaction
	queriedID []string
}

func (s *countingReactionLookup) FindByUserAndNoteIDs(_ string, noteIDs []string) (map[string]*model.NoteReaction, error) {
	s.queriedID = append(s.queriedID, noteIDs...)
	out := make(map[string]*model.NoteReaction, len(noteIDs))
	for _, nid := range noteIDs {
		if r, ok := s.rows[nid]; ok {
			out[nid] = r
		}
	}
	return out, nil
}

func TestNoteFieldResolver_MyReaction_SkipsZeroReactionNotes(t *testing.T) {
	reactions := &countingReactionLookup{rows: map[string]*model.NoteReaction{
		"hot": {NoteID: "hot", Reaction: "👍"},
	}}
	r := NewNoteFieldResolver(nil, nil, nil, reactions, nil, makeIDGen(t))

	notes := []NoteEntity{
		{ID: "hot", ReactionCount: 3},  // reaction あり → fetch 対象
		{ID: "cold", ReactionCount: 0}, // reaction 無し → skip
	}
	r.ResolveViewerFields(notes, &model.User{ID: "v1"})

	// fetch されたのは reactionCount>0 の "hot" のみ
	assert.Equal(t, []string{"hot"}, reactions.queriedID)
	require.NotNil(t, notes[0].MyReaction)
	assert.Nil(t, notes[1].MyReaction, "zero-reaction note must not get myReaction")
}

// #2735: resolver が viewer から読むのは ID だけであることを固定する。
// entity.WithNoteFieldResolver は notifiee ID から `&model.User{ID: ...}` を
// 組み立てて渡す (pack のためだけに user 行を引き直さない) ので、resolver が
// ID 以外のフィールドを見るようになるとその前提が黙って壊れる。
func TestResolveViewerFields_UsesViewerIDOnly(t *testing.T) {
	newNotes := func() []NoteEntity {
		return []NoteEntity{{
			ID: "n1", ReactionCount: 1,
			Poll: &PollEntity{Choices: []PollChoice{{Text: "a"}, {Text: "b"}}},
		}}
	}
	reactions := &recordingReactionLookup{rows: map[string]*model.NoteReaction{
		"n1": {NoteID: "n1", Reaction: "👍"},
	}}
	votes := &recordingPollVoteLookup{votes: map[string][]int{"n1": {1}}}
	r := NewNoteFieldResolver(nil, nil, nil, reactions, nil, makeIDGen(t))
	r.SetPollVoteLookup(votes)

	// ID だけを持つ stub と、同じ ID で他フィールドが埋まった user の出力が
	// 一致すること。
	stub := newNotes()
	r.ResolveViewerFields(stub, &model.User{ID: "viewer"})

	// full 側は **ID 以外の値を ID と別物にしておく**。同じ文字列だと、resolver が
	// Username を lookup key に使い始めても検出できない。
	full := newNotes()
	r.ResolveViewerFields(full, &model.User{
		ID: "viewer", Username: "viewer_name", UsernameLower: "viewer_name",
		IsBot: true,
	})

	// **lookup key が ID であること自体を見る。** 出力の一致だけだと、resolver が
	// viewer.Username を key に使い始めても (stub が userID を無視するので) 検出できない。
	assert.Equal(t, "viewer", reactions.gotUserID, "note_reaction は viewer.ID で引くこと")
	assert.Equal(t, "viewer", votes.gotUserID, "poll_vote は viewer.ID で引くこと")

	require.NotNil(t, stub[0].MyReaction, "positive control: myReaction が解決されていること")
	assert.Equal(t, full[0].MyReaction, stub[0].MyReaction)
	require.NotNil(t, stub[0].Poll)
	require.Len(t, stub[0].Poll.Choices, 2)
	assert.True(t, stub[0].Poll.Choices[1].IsVoted, "positive control: isVoted が解決されていること")
	assert.Equal(t, full[0].Poll.Choices, stub[0].Poll.Choices)
}

// recordingReactionLookup records the userID it was queried with so tests can
// assert that the resolver keys the lookup on viewer.ID.
type recordingReactionLookup struct {
	rows      map[string]*model.NoteReaction
	gotUserID string
}

func (r *recordingReactionLookup) FindByUserAndNoteIDs(userID string, _ []string) (map[string]*model.NoteReaction, error) {
	r.gotUserID = userID
	return r.rows, nil
}

// recordingPollVoteLookup is the PollVoteLookup counterpart of
// recordingReactionLookup.
type recordingPollVoteLookup struct {
	votes     map[string][]int
	gotUserID string
}

func (r *recordingPollVoteLookup) FindByUserAndNoteIDs(userID string, _ []string) (map[string][]int, error) {
	r.gotUserID = userID
	return r.votes, nil
}

// ResolveFilesShallow は folder / owner の lookup が配線されていても embed しない
// (#2737)。Web Push の payload は約 4 KB が上限で、embed は 1 件あたり 312-367 B を
// 積み増すだけで使われないため。upstream の packManyByIds (オプション無し) が
// folder / user を embed しないのと揃える (shape 全体が同じという意味ではない)。
//
// **folder / owner lookup を配線して比べること。** 配線しない状態では
// ResolveFiles も folder / user を nil にするので、両者の差が出ない。
func TestNoteFieldResolver_ResolveFilesShallow_OmitsFolderAndOwner(t *testing.T) {
	folderID := "folder-1"
	owner := "user-1"
	newFiles := func() *stubDriveFileLookup {
		return &stubDriveFileLookup{files: map[string]*model.DriveFile{
			"f1": {ID: "f1", FolderID: &folderID, UserID: &owner, Name: "x.png", Type: "image/png", URL: "https://example.com/f1"},
		}}
	}
	folders := &stubFolderLookup{folders: map[string]*model.DriveFolder{folderID: {ID: folderID, Name: "media"}}}
	owners := &stubFileOwnerLookup{users: map[string]*model.User{owner: {ID: owner, Username: "alice"}}}

	deep := NewNoteFieldResolver(newFiles(), folders, owners, nil, nil, makeIDGen(t))
	deepNotes := []NoteEntity{{ID: "n1", FileIDs: model.StringArray{"f1"}}}
	deep.ResolveFiles(deepNotes)
	require.Len(t, deepNotes[0].Files, 1)
	deepFile := deepNotes[0].Files[0].(DriveFileEntity)
	require.NotNil(t, deepFile.Folder, "ResolveFiles は folder を embed する")
	require.NotNil(t, deepFile.User, "ResolveFiles は owner を embed する")

	shallow := NewNoteFieldResolver(newFiles(), folders, owners, nil, nil, makeIDGen(t))
	notes := []NoteEntity{{ID: "n1", FileIDs: model.StringArray{"f1"}}}
	shallow.ResolveFilesShallow(notes)
	require.Len(t, notes[0].Files, 1)
	f := notes[0].Files[0].(DriveFileEntity)
	assert.Nil(t, f.Folder, "shallow は folder を embed しない")
	assert.Nil(t, f.User, "shallow は owner を embed しない")
	require.NotNil(t, f.FolderID)
	assert.Equal(t, folderID, *f.FolderID, "ID 自体は upstream と同じく出す")
}

// shallow でも embed (renote / reply) の files を埋める。push 本文の要約は
// renote 通知で renote 側を、reply では "RE: ..." を見るため。
func TestNoteFieldResolver_ResolveFilesShallow_EmbedsRenoteAndReply(t *testing.T) {
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", Name: "a.png", Type: "image/png", URL: "https://example.com/f1"},
		"f2": {ID: "f2", Name: "b.png", Type: "image/png", URL: "https://example.com/f2"},
		"f3": {ID: "f3", Name: "c.png", Type: "image/png", URL: "https://example.com/f3"},
	}}
	r := NewNoteFieldResolver(files, nil, nil, nil, nil, makeIDGen(t))
	notes := []NoteEntity{{
		ID:      "n1",
		FileIDs: model.StringArray{"f1"},
		Renote:  &NoteEntity{ID: "n2", FileIDs: model.StringArray{"f2"}},
		Reply:   &NoteEntity{ID: "n3", FileIDs: model.StringArray{"f3"}},
	}}
	r.ResolveFilesShallow(notes)
	assert.Len(t, notes[0].Files, 1)
	assert.Len(t, notes[0].Renote.Files, 1)
	assert.Len(t, notes[0].Reply.Files, 1)
}

// lookup 未配線 / nil レシーバは no-op。
func TestNoteFieldResolver_ResolveFilesShallow_NoOp(t *testing.T) {
	var nilResolver *NoteFieldResolver
	nilResolver.ResolveFilesShallow([]NoteEntity{{ID: "n1"}})

	r := NewNoteFieldResolver(nil, nil, nil, nil, nil, makeIDGen(t))
	notes := []NoteEntity{{ID: "n1", FileIDs: model.StringArray{"f1"}}}
	r.ResolveFilesShallow(notes)
	assert.Empty(t, notes[0].Files)
}
