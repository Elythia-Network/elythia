package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

type stubMediaSilenced struct{ hosts map[string]bool }

func (s stubMediaSilenced) IsMediaSilenced(host string) bool { return s.hosts[host] }

// メディアサイレンスのホストの添付はセンシティブにする (upstream の
// DriveService.addFile と同じ)。maybeSensitive は検出の結果なので触らない。
func TestIngestNote_MediaSilencedHostMarksAttachmentsSensitive(t *testing.T) {
	r, _, files := newRuleResolver(t, nil)
	r.SetMediaSilencedHostChecker(stubMediaSilenced{hosts: map[string]bool{"remote.example": true}})
	victim := "victim-id"
	files.Files["other"] = &model.DriveFile{ID: "other", UserID: &victim, URI: strPtr("https://remote.example/files/o.png"), URL: "https://remote.example/files/o.png"}

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "look",
		"attachment": [{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/a.png"},
		{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/o.png"}]`)), "")
	require.NoError(t, err)
	require.Len(t, note.FileIDs, 2)
	created := files.Files[note.FileIDs[0]]
	assert.True(t, created.IsSensitive)
	assert.False(t, created.MaybeSensitive, "maybeSensitive is a detection result and stays as is")
	assert.False(t, files.Files["other"].IsSensitive, "another user's file is not touched")
	require.NotNil(t, note.CW, "a file that cannot be marked folds the note instead")
	assert.Equal(t, "", *note.CW)

	// 投稿者自身が前に添付した行 (設定する前に取り込んだもの) にも当てる。
	author := note.UserID
	files.Files["own"] = &model.DriveFile{ID: "own", UserID: &author, URI: strPtr("https://remote.example/files/b.png"), URL: "https://remote.example/files/b.png"}
	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n2","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"again",
		"attachment":[{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/b.png"}]}`), "")
	require.NoError(t, err)
	assert.True(t, files.Files["own"].IsSensitive)
	assert.Nil(t, note.CW, "every file is sensitive, so the note is not folded")

	// 持ち主のホストもメディアサイレンス対象なら、他人の行でも書き換える
	// (本来センシティブであるべき行)。
	silencedHost := "remote.example"
	neighbour := "neighbour-id"
	files.Files["neigh"] = &model.DriveFile{ID: "neigh", UserID: &neighbour, UserHost: &silencedHost,
		URI: strPtr("https://remote.example/files/n.png"), URL: "https://remote.example/files/n.png"}
	note, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n3","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"neighbour",
		"attachment":[{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/n.png"}]}`), "")
	require.NoError(t, err)
	assert.True(t, files.Files["neigh"].IsSensitive)
	assert.Nil(t, note.CW)
}

// ルールの「センシティブにする」と重なっても、ルールの maybeSensitive は付く
// (メディアサイレンスを先に当てると、isSensitive だけ立った行を済みとして飛ばす)。
func TestIngestNote_MediaSilencedAndRuleSensitive(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Sensitive: true})
	r, _, files := newRuleResolver(t, svc)
	r.SetMediaSilencedHostChecker(stubMediaSilenced{hosts: map[string]bool{"remote.example": true}})
	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "x", `+ruleImage)), "")
	require.NoError(t, err)
	require.Len(t, note.FileIDs, 1)
	f := files.Files[note.FileIDs[0]]
	assert.True(t, f.IsSensitive)
	assert.True(t, f.MaybeSensitive, "the rule's maybeSensitive is kept")
	assert.Nil(t, note.CW)

	// 再利用する投稿者自身の行でも同じ。
	author := note.UserID
	files.Files["own"] = &model.DriveFile{ID: "own", UserID: &author, URI: strPtr("https://remote.example/files/b.png"), URL: "https://remote.example/files/b.png"}
	_, _, err = r.IngestNoteWithCreated([]byte(`{"id":"https://remote.example/notes/n2","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"again",
		"attachment":[{"type":"Document","mediaType":"image/png","url":"https://remote.example/files/b.png"}]}`), "")
	require.NoError(t, err)
	assert.True(t, files.Files["own"].IsSensitive)
	assert.True(t, files.Files["own"].MaybeSensitive)
}

// 対象でないホストは変わらない。未配線でも変わらない。
func TestIngestNote_NotMediaSilencedHostKeepsAttachments(t *testing.T) {
	for name, checker := range map[string]*stubMediaSilenced{
		"other host": {hosts: map[string]bool{"elsewhere.example": true}},
		"not wired":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			r, _, files := newRuleResolver(t, nil)
			if checker != nil {
				r.SetMediaSilencedHostChecker(*checker)
			}
			note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "x", `+ruleImage)), "")
			require.NoError(t, err)
			require.Len(t, note.FileIDs, 1)
			assert.False(t, files.Files[note.FileIDs[0]].IsSensitive)
		})
	}
}

// 編集の取り込みでも効く。
func TestUpdateRemoteNote_MediaSilencedHostMarksAttachmentsSensitive(t *testing.T) {
	r, _, files := newRuleUpdateResolver(t, nil)
	r.SetMediaSilencedHostChecker(stubMediaSilenced{hosts: map[string]bool{"remote.example": true}})
	// 編集前から付いていた投稿者自身の行 (再利用される) にも当てる。
	author := "alice-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &author, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}
	got, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	require.Equal(t, model.StringArray{"pre"}, got.FileIDs)
	assert.True(t, files.Files["pre"].IsSensitive)
	assert.Nil(t, got.CW)
}

// 編集でも、書き換えられない他人の行が残れば投稿ごと畳む。
func TestUpdateRemoteNote_MediaSilencedFoldsWhenAFileCannotBeMarked(t *testing.T) {
	r, noteRepo, files := newRuleUpdateResolver(t, nil)
	r.SetMediaSilencedHostChecker(stubMediaSilenced{hosts: map[string]bool{"remote.example": true}})
	victim := "victim-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &victim, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}
	got, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	assert.False(t, files.Files["pre"].IsSensitive)
	require.NotNil(t, got.CW)
	assert.Equal(t, "", *got.CW)
	require.NotNil(t, noteRepo.Notes["n1"].CW)
}

// 再利用する行でも、ルールの maybeSensitive が付く (順序の差が出るのはこちら。
// 新しく作る行は作る時点で両方立つ)。
func TestUpdateRemoteNote_MediaSilencedAndRuleSensitiveOnAReusedFile(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Sensitive: true})
	r, _, files := newRuleUpdateResolver(t, svc)
	r.SetMediaSilencedHostChecker(stubMediaSilenced{hosts: map[string]bool{"remote.example": true}})
	author := "alice-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &author, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}
	_, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	assert.True(t, files.Files["pre"].IsSensitive)
	assert.True(t, files.Files["pre"].MaybeSensitive, "the rule's maybeSensitive is kept")
}

// 持ち主の無い行 (リレー由来で著者の行が無いとき、upsertAttachments は userId を
// 空にして作る) も、userHost がメディアサイレンス対象なら書き換える。これが
// 効かないと、対象ホストのリレー投稿が全部畳まれる。
func TestIngestNote_MediaSilencedMarksOwnerlessRowOfTheHost(t *testing.T) {
	r, _, files := newRuleResolver(t, nil)
	r.SetMediaSilencedHostChecker(stubMediaSilenced{hosts: map[string]bool{"remote.example": true}})
	host := "remote.example"
	files.Files["relay"] = &model.DriveFile{ID: "relay", UserHost: &host, URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}
	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "x", `+ruleImage)), "")
	require.NoError(t, err)
	require.Equal(t, model.StringArray{"relay"}, note.FileIDs)
	assert.True(t, files.Files["relay"].IsSensitive)
	assert.Nil(t, note.CW, "not folded")
}

// 別の取り込みで isSensitive だけ立った行にも、ルールの maybeSensitive は付く。
func TestUpdateRemoteNote_RuleSensitiveCompletesAMediaSilencedRow(t *testing.T) {
	svc, _ := newRules(t, &model.FederationRule{ID: "r1", Hosts: remoteHost(), Sensitive: true})
	r, _, files := newRuleUpdateResolver(t, svc)
	author := "alice-id"
	files.Files["pre"] = &model.DriveFile{ID: "pre", UserID: &author, IsSensitive: true,
		URI: strPtr("https://remote.example/files/a.png"), URL: "https://remote.example/files/a.png"}
	_, err := r.UpdateRemoteNote([]byte(ruleUpdateBody), "")
	require.NoError(t, err)
	assert.True(t, files.Files["pre"].MaybeSensitive)
}
