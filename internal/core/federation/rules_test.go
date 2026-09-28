package federation_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/core/fedrule"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

type ruleStore struct{ rules []*model.FederationRule }

func (s *ruleStore) List() ([]*model.FederationRule, error) { return s.rules, nil }

type ruleHits struct{ hits []fedrule.Hit }

func (r *ruleHits) Record(h fedrule.Hit) { r.hits = append(r.hits, h) }

func newRules(t *testing.T, rules ...*model.FederationRule) (*fedrule.Service, *ruleHits) {
	t.Helper()
	for _, r := range rules {
		if r.Mode == "" {
			r.Mode = model.FederationRuleModeEnforce
		}
		if r.Target == "" {
			r.Target = model.FederationRuleTargetNote
		}
		require.NoError(t, fedrule.Normalize(r))
	}
	hits := &ruleHits{}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return fedrule.NewService(&ruleStore{rules: rules}, hits, idGen.ParseTime), hits
}

func newRuleResolver(t *testing.T, svc federation.RuleEvaluator) (*federation.Resolver, *testutil.MockNoteRepository, *testutil.MockDriveFileRepository) {
	t.Helper()
	r, noteRepo := newProhibitedWordsResolver(t, nil)
	files := testutil.NewMockDriveFileRepository()
	r.SetDriveFileRepo(files)
	if svc != nil {
		r.SetRuleEvaluator(svc)
	}
	return r, noteRepo, files
}

const ruleImage = `"attachment": [{"type": "Document", "mediaType": "image/png", "url": "https://remote.example/files/a.png"}]`

func remoteHost() model.StringArray { return model.StringArray{"remote.example"} }

func TestIngestNote_RuleRejects(t *testing.T) {
	svc, hits := newRules(t, &model.FederationRule{ID: "r1", Patterns: model.StringArray{"buy now"}, Reject: true})
	r, noteRepo, files := newRuleResolver(t, svc)

	note, created, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "buy now!!", `+ruleImage)), "")
	require.NoError(t, err, "acked, not retried")
	assert.Nil(t, note)
	assert.False(t, created)
	assert.Empty(t, noteRepo.Notes)
	assert.Empty(t, files.Files, "no attachment rows for a rejected note")
	require.Len(t, hits.hits, 1)
	assert.Equal(t, "r1", hits.hits[0].RuleID)
	assert.Equal(t, "remote.example", hits.hits[0].Host)
	assert.Equal(t, "https://remote.example/notes/n1", hits.hits[0].Subject)
	assert.True(t, hits.hits[0].Applied)

	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n2","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"fine"}`), "")
	require.NoError(t, err)
	assert.NotNil(t, note, "notes that do not match are stored")
}

// record は記録だけで、投稿はそのまま取り込む。
func TestIngestNote_RuleRecordOnly(t *testing.T) {
	svc, hits := newRules(t, &model.FederationRule{ID: "r1", Mode: model.FederationRuleModeRecord,
		Hosts: remoteHost(), Reject: true, Unlist: true, CW: strPtr("x")})
	r, noteRepo, _ := newRuleResolver(t, svc)

	note, created, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "hello"`)), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.True(t, created)
	assert.Equal(t, model.NoteVisibilityPublic, note.Visibility)
	assert.Nil(t, note.CW)
	assert.Len(t, noteRepo.Notes, 1)
	require.Len(t, hits.hits, 1)
	assert.False(t, hits.hits[0].Applied)
}

func TestIngestNote_RuleRewrites(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Unlist: true, CW: strPtr("#ruletag 注意"), Sensitive: true})
	r, _, files := newRuleResolver(t, svc)

	// 同じ URL の添付を別の利用者が先に持っている。**他人の行は書き換えない**
	// (再利用は持ち主を見ないので、URL を指すだけで他人のファイルを
	// センシティブにできてしまう)。
	victim := "victim-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &victim, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "hello #mine", `+ruleImage)), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, model.NoteVisibilityHome, note.Visibility, "unlisted like a silenced host")
	require.NotNil(t, note.CW)
	assert.Equal(t, "#ruletag 注意", *note.CW)
	assert.Equal(t, model.StringArray{"mine"}, note.Tags, "the rule's CW does not add tags")
	require.Equal(t, model.StringArray{"pre"}, note.FileIDs)
	assert.False(t, files.Files["pre"].IsSensitive, "another user's file is not touched")

	// 投稿者自身が前に添付した行 (同じ画像の再投稿) はセンシティブにする。
	author := note.UserID
	files.Files["own"] = &model.DriveFile{ID: "own", UserID: &author, URI: strPtr("https://remote.example/files/b.png"), URL: "https://remote.example/files/b.png"}
	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n2","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"again",
		"attachment":[{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/b.png"}]}`), "")
	require.NoError(t, err)
	require.Equal(t, model.StringArray{"own"}, note.FileIDs)
	assert.True(t, files.Files["own"].IsSensitive, "the author's reused attachment is marked sensitive")
	assert.True(t, files.Files["own"].MaybeSensitive)

	// 新しく作る添付の行は最初からセンシティブ。
	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n3","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"new",
		"attachment":[{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/c.png"}]}`), "")
	require.NoError(t, err)
	require.Len(t, note.FileIDs, 1)
	assert.True(t, files.Files[note.FileIDs[0]].IsSensitive)
}

// 投票の選択肢もパターンの対象 (本文を無害にして選択肢に書くだけで素通りさせない)。
func TestIngestNote_RulePatternSeesPollChoices(t *testing.T) {
	svc, hits := newRules(t, &model.FederationRule{ID: "r1", Patterns: model.StringArray{"buy now"}, Reject: true})
	r, noteRepo, _ := newRuleResolver(t, svc)
	body := remoteNoteBody(`"content": "vote please", "oneOf": [{"type":"Note","name":"ok"},{"type":"Note","name":"buy now"}]`)
	note, _, err := r.IngestNoteWithCreated([]byte(body), "")
	require.NoError(t, err)
	assert.Nil(t, note)
	assert.Empty(t, noteRepo.Notes)
	assert.Len(t, hits.hits, 1)
}

// 送信者の CW は上書きしない。sensitive だけの空の CW には文言を補う。
func TestIngestNote_RuleCWKeepsTheSendersCW(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), CW: strPtr("rule")})
	r, _, _ := newRuleResolver(t, svc)

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "a", "summary": "sender"`)), "")
	require.NoError(t, err)
	assert.Equal(t, "sender", *note.CW)

	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n2","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"b","sensitive":true}`), "")
	require.NoError(t, err)
	assert.Equal(t, "rule", *note.CW)
}

func TestIngestNote_RuleStripsMediaAndMatchesAttachmentsAndTags(t *testing.T) {
	svc, hits := newRules(t,
		&model.FederationRule{ID: "media", HasAttachment: strPtrBool(true), StripMedia: true},
		&model.FederationRule{ID: "tag", Tags: model.StringArray{"#Spam"}, Mode: model.FederationRuleModeRecord, Reject: true},
	)
	r, _, files := newRuleResolver(t, svc)

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "look", "tag": [{"type":"Hashtag","name":"#SPAM"}], `+ruleImage)), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Empty(t, note.FileIDs)
	assert.Empty(t, files.Files, "stripped attachments are not stored")
	require.Len(t, hits.hits, 2, "the tag from the AP tag array is normalized and matched")

	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n2","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"no media"}`), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Len(t, hits.hits, 2, "hasAttachment=true does not match a note without attachments")
}

// ルールが無ければ何も変えない (配線されていても評価を省く)。
func TestIngestNote_NoRules(t *testing.T) {
	svc, _ := newRules(t)
	r, _, files := newRuleResolver(t, svc)
	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "x", `+ruleImage)), "")
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityPublic, note.Visibility)
	require.Len(t, note.FileIDs, 1)
	assert.False(t, files.Files[note.FileIDs[0]].IsSensitive)
}

func newRuleUpdateResolver(t *testing.T, svc federation.RuleEvaluator) (*federation.Resolver, *testutil.MockNoteRepository, *testutil.MockDriveFileRepository) {
	t.Helper()
	r, noteRepo, _ := newProhibitedWordsUpdateResolver(t, nil)
	files := testutil.NewMockDriveFileRepository()
	r.SetDriveFileRepo(files)
	if svc != nil {
		r.SetRuleEvaluator(svc)
	}
	noteRepo.Notes["n1"].Visibility = model.NoteVisibilityPublic
	return r, noteRepo, files
}

const ruleUpdateBody = `{"@context":"https://www.w3.org/ns/activitystreams",
	"id":"https://remote.example/notes/n1","type":"Note",
	"attributedTo":"https://remote.example/users/alice",
	"content":"now buy now #edit", ` + ruleImage + `}`

// 編集にも掛かる。掛けないと、当たらない投稿を作ってから差し替えれば素通りできる。
func TestUpdateRemoteNote_RuleRejects(t *testing.T) {
	svc, hits := newRules(t, &model.FederationRule{ID: "r1", Patterns: model.StringArray{"buy now"}, Reject: true})
	r, noteRepo, files := newRuleUpdateResolver(t, svc)

	got, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "original", *noteRepo.Notes["n1"].Text)
	assert.Empty(t, noteRepo.UpdateFieldsCalls)
	assert.Empty(t, files.Files)
	require.Len(t, hits.hits, 1)
	assert.Equal(t, "Update", hits.hits[0].Kind)
	assert.Equal(t, "remote.example", hits.hits[0].Host, "the host comes from the note when the author row is missing")
}

func TestUpdateRemoteNote_RuleRewrites(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Unlist: true, CW: strPtr("rule"), Sensitive: true})
	r, noteRepo, files := newRuleUpdateResolver(t, svc)
	// 編集前から付いていた添付 (同じ URL の行を再利用する) にも効くこと。
	author := "alice-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &author, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}

	got, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityHome, got.Visibility)
	require.NotNil(t, got.CW)
	assert.Equal(t, "rule", *got.CW)
	assert.Equal(t, model.StringArray{"edit"}, got.Tags)
	require.Equal(t, model.StringArray{"pre"}, got.FileIDs)
	assert.True(t, files.Files["pre"].IsSensitive, "a reused attachment is marked sensitive too")
	assert.Equal(t, model.NoteVisibilityHome, noteRepo.Notes["n1"].Visibility)
	assert.Equal(t, "rule", *noteRepo.Notes["n1"].CW)
}

func TestUpdateRemoteNote_RuleStripsMedia(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), StripMedia: true})
	r, _, files := newRuleUpdateResolver(t, svc)
	got, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	assert.Empty(t, got.FileIDs)
	assert.Empty(t, files.Files)
}

// activity のルールは種別で拒否する。拒否したものは ack して捨てる。
func TestProcess_ActivityRuleRejects(t *testing.T) {
	svc, hits := newRules(t, &model.FederationRule{ID: "follow", Target: model.FederationRuleTargetActivity,
		ActivityTypes: model.StringArray{"Follow"}, Hosts: remoteHost(), Reject: true})
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	p.SetRuleEvaluator(svc)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type": "Follow", "id": "https://remote.example/follows/1",
		"actor": "https://remote.example/users/alice", "object": "https://example.com/users/bob"}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, followingRepo.Followings)
	require.Len(t, hits.hits, 1)
	assert.Equal(t, "https://remote.example/follows/1", hits.hits[0].Subject)
	assert.Equal(t, "Follow", hits.hits[0].Kind)

	// 署名者 = actor なら、その行で bot / 新規を判定する (DB を引き直さない)。
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	signer := &model.User{ID: "alice", URI: &aliceURI, Host: &host, IsBot: true}
	svc2, hits2 := newRules(t, &model.FederationRule{ID: "bots", Target: model.FederationRuleTargetActivity,
		ActivityTypes: model.StringArray{"Follow"}, IsBot: strPtrBool(true), Reject: true})
	p.SetRuleEvaluator(svc2)
	require.NoError(t, p.ProcessWithSigner(body, signer))
	assert.Empty(t, followingRepo.Followings)
	assert.Len(t, hits2.hits, 1)

	// 当たらない activity は通す。
	signer.IsBot = false
	require.NoError(t, p.ProcessWithSigner(body, signer))
	assert.Len(t, followingRepo.Followings, 1)
}

// Collection の中身は 1 件ずつ種別で評価する。
func TestProcess_ActivityRuleSeesCollectionItems(t *testing.T) {
	svc, hits := newRules(t, &model.FederationRule{ID: "follow", Target: model.FederationRuleTargetActivity,
		ActivityTypes: model.StringArray{"Follow"}, Reject: true})
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	p.SetRuleEvaluator(svc)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	body := []byte(`{"type": "Collection", "id": "https://remote.example/c/1", "actor": "https://remote.example/users/alice",
		"items": [{"type": "Follow", "id": "https://remote.example/follows/2", "actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"}]}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, followingRepo.Followings)
	assert.Len(t, hits.hits, 1)
}

func strPtrBool(b bool) *bool { return &b }

// 保存済みの CW にルールの文言が入っていても、編集では送信者の CW として
// 扱わない (Update の summary を正とする)。扱うと文言の hashtag が tag に
// 拾われ、文言が他のルールのパターンに当たって以後の編集が全部弾かれる。
func TestUpdateRemoteNote_StoredRuleCWIsNotTheSendersCW(t *testing.T) {
	svc, hits := newRules(t,
		&model.FederationRule{ID: "cw", Hosts: remoteHost(), CW: strPtr("#ruletag spam warning")},
		&model.FederationRule{ID: "spam", Patterns: model.StringArray{"spam"}, Reject: true},
	)
	r, noteRepo, _ := newRuleUpdateResolver(t, svc)
	noteRepo.Notes["n1"].CW = strPtr("#ruletag spam warning") // 取り込み時にルールが付けた

	body := `{"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"edited #mine"}`
	got, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "edited #mine", *got.Text, "a harmless edit is applied")
	assert.Equal(t, model.StringArray{"mine"}, got.Tags, "the rule's CW does not become tags")
	require.NotNil(t, got.CW)
	assert.Equal(t, "#ruletag spam warning", *got.CW, "the rule adds its CW again")
	for _, h := range hits.hits {
		assert.NotEqual(t, "spam", h.RuleID, "the rule's own wording is not matched against patterns")
	}
}

// ルールが無くなった後の編集では、ルールの CW も外れる。送信者が編集で CW を
// 外したときも外れる。
func TestUpdateRemoteNote_CWFollowsTheUpdate(t *testing.T) {
	r, noteRepo, _ := newRuleUpdateResolver(t, nil)
	noteRepo.Notes["n1"].CW = strPtr("old cw")

	got, err := r.UpdateRemoteNote([]byte(`{"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"no cw now"}`), "")
	require.NoError(t, err)
	assert.Nil(t, got.CW)
	assert.Nil(t, noteRepo.Notes["n1"].CW)

	got, err = r.UpdateRemoteNote([]byte(`{"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"cw again","summary":"new cw"}`), "")
	require.NoError(t, err)
	require.NotNil(t, got.CW)
	assert.Equal(t, "new cw", *got.CW)
}

// 「初めて見てから N 時間以内」は、編集では投稿した時刻で測る。評価した時刻で
// 測ると、時間が経ってから編集するだけでルールの CW が外れる。
func TestUpdateRemoteNote_NewAccountRuleIsMeasuredAtPostTime(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	authorID := idGen.Generate(t0)
	noteID := idGen.Generate(t0.Add(time.Hour))

	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	userRepo.Users[authorID] = &model.User{ID: authorID, Username: "alice", Host: &host, URI: &aliceURI}
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	text := "hi"
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, URI: &uri, UserID: authorID, UserHost: &host, Text: &text,
		CW: strPtr("new account"), Visibility: model.NoteVisibilityHome}
	r := federation.NewResolver(userRepo, noteRepo, activitypub.NewURLBuilder("https://example.com"), &stubFetcher{}, idGen)

	svc, _ := newRules(t, &model.FederationRule{ID: "new", NewWithinHours: ptrInt(24), CW: strPtr("new account"), Unlist: true})
	svc.SetClockForTest(func() time.Time { return t0.Add(48 * time.Hour) })
	r.SetRuleEvaluator(svc)

	got, err := r.UpdateRemoteNote([]byte(`{"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"hi, edited"}`), "")
	require.NoError(t, err)
	require.NotNil(t, got.CW, "the edit two days later still carries the rule's CW")
	assert.Equal(t, "new account", *got.CW)
}

// 他人の行を再利用する添付は書き換えられないので、投稿ごと空の CW で畳む
// (そのままだと「センシティブにする」が効かない)。
func TestIngestNote_RuleSensitiveFoldsWhenAFileCannotBeMarked(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Sensitive: true})
	r, _, files := newRuleResolver(t, svc)
	victim := "victim-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &victim, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "look", `+ruleImage)), "")
	require.NoError(t, err)
	require.NotNil(t, note.CW, "folded")
	assert.Equal(t, "", *note.CW)
	assert.False(t, files.Files["pre"].IsSensitive)

	// 自分の添付だけなら畳まない (添付がセンシティブになるので足りる)。
	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n2","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"mine",
		"attachment":[{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/new.png"}]}`), "")
	require.NoError(t, err)
	assert.Nil(t, note.CW)
	assert.True(t, files.Files[note.FileIDs[0]].IsSensitive)
}

func TestUpdateRemoteNote_RuleSensitiveFoldsWhenAFileCannotBeMarked(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Sensitive: true})
	r, noteRepo, files := newRuleUpdateResolver(t, svc)
	victim := "victim-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &victim, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}
	got, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	require.NotNil(t, got.CW)
	assert.Equal(t, "", *got.CW)
	require.NotNil(t, noteRepo.Notes["n1"].CW)
}

// タグの条件は note.tags の 32 個の上限で打ち切らずに見る。打ち切ると、tag 配列を
// 詰め物で埋めて本文の #tag を押し出すだけですり抜けられる。
func TestIngestNote_RuleTagsAreNotCapped(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Tags: model.StringArray{"spamtag"}, Reject: true})
	r, noteRepo, _ := newRuleResolver(t, svc)
	var pad []string
	for i := 0; i < 40; i++ {
		pad = append(pad, fmt.Sprintf(`{"type":"Hashtag","name":"#pad%d"}`, i))
	}
	body := remoteNoteBody(`"content": "buy #spamtag", "tag": [` + strings.Join(pad, ",") + `]`)
	note, _, err := r.IngestNoteWithCreated([]byte(body), "")
	require.NoError(t, err)
	assert.Nil(t, note)
	assert.Empty(t, noteRepo.Notes)
}

func ptrInt(v int) *int { return &v }

// 同じ URL の添付が 2 つあっても (同じ行が 2 回並ぶ)、全部自分の行なら畳まない。
func TestIngestNote_RuleSensitiveDuplicateAttachmentsDoNotFold(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Sensitive: true})
	r, _, files := newRuleResolver(t, svc)
	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "twice",
		"attachment": [{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/d.png"},
		{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/d.png"}]`)), "")
	require.NoError(t, err)
	require.Len(t, note.FileIDs, 2)
	assert.Equal(t, note.FileIDs[0], note.FileIDs[1])
	assert.Nil(t, note.CW)
	assert.True(t, files.Files[note.FileIDs[0]].IsSensitive)
}
