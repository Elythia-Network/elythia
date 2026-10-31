package federation_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// publicNotePolicies is a RolePolicyProvider whose canPublicNote is fixed. It
// records the users it was asked about.
type publicNotePolicies struct {
	mayBeFalse    bool
	canPublicNote any
	asked         []string
}

func (p *publicNotePolicies) GetUserPolicies(userID string) map[string]any {
	p.asked = append(p.asked, userID)
	return map[string]any{"canPublicNote": p.canPublicNote}
}

func (p *publicNotePolicies) BoolPolicyMayBeFalse(key string) bool {
	return key == "canPublicNote" && p.mayBeFalse
}

// 本家はリモートの投稿も NoteCreateService.create を通すので、投稿者のロールが
// canPublicNote を許さなければ public を home にする (#3369)。
func TestIngestNote_CanPublicNoteDemotesPublicToHome(t *testing.T) {
	tests := []struct {
		name      string
		policies  *publicNotePolicies
		body      string
		want      model.NoteVisibility
		wantAsked bool
	}{
		{name: "denied", policies: &publicNotePolicies{mayBeFalse: true, canPublicNote: false},
			body: remoteNoteBody(`"content": "hello"`), want: model.NoteVisibilityHome, wantAsked: true},
		{name: "allowed", policies: &publicNotePolicies{mayBeFalse: true, canPublicNote: true},
			body: remoteNoteBody(`"content": "hello"`), want: model.NoteVisibilityPublic, wantAsked: true},
		// 本家は `=== false` で見るので、真偽値でない値では降格しない
		{name: "not a boolean", policies: &publicNotePolicies{mayBeFalse: true, canPublicNote: "no"},
			body: remoteNoteBody(`"content": "hello"`), want: model.NoteVisibilityPublic, wantAsked: true},
		// 誰にも当たりえないときは、投稿者の policy を引かない
		{name: "no role can deny", policies: &publicNotePolicies{mayBeFalse: false, canPublicNote: false},
			body: remoteNoteBody(`"content": "hello"`), want: model.NoteVisibilityPublic, wantAsked: false},
		// public でない投稿は見ない
		{name: "followers only", policies: &publicNotePolicies{mayBeFalse: true, canPublicNote: false},
			body: `{"@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/notes/n1", "type": "Note",
				"attributedTo": "https://remote.example/users/alice",
				` + followersOnlyAudience + `, "content": "hello"}`,
			want: model.NoteVisibilityFollowers, wantAsked: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, noteRepo := newProhibitedWordsResolver(t, nil)
			r.SetRolePolicyProviderForTest(tc.policies)

			note, created, err := r.IngestNoteWithCreated([]byte(tc.body), "")
			require.NoError(t, err)
			require.NotNil(t, note)
			assert.True(t, created)
			assert.Equal(t, tc.want, note.Visibility)
			require.Len(t, noteRepo.Notes, 1)
			if tc.wantAsked {
				assert.Equal(t, []string{note.UserID}, tc.policies.asked, "投稿者の policy を引く")
			} else {
				assert.Empty(t, tc.policies.asked)
			}
		})
	}
}

// センシティブワードの判定が先で、当たればロールは見ない (本家の else if の順)。
func TestIngestNote_SensitiveWordsCheckedBeforeCanPublicNote(t *testing.T) {
	r, _ := newProhibitedWordsResolver(t, nil)
	r.SetHostBlockerForTest(&stubHostBlocker{sensitive: []string{"spoiler"}})
	p := &publicNotePolicies{mayBeFalse: true, canPublicNote: false}
	r.SetRolePolicyProviderForTest(p)

	note, _, err := r.IngestNoteWithCreated([]byte(remoteNoteBody(`"content": "a spoiler here"`)), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, model.NoteVisibilityHome, note.Visibility)
	assert.Empty(t, p.asked)
}

// 編集 (Update) の取り込みでも、投稿者のロールが canPublicNote を許さなければ
// public を home にする。狭める方向だけ。
func TestUpdateRemoteNote_CanPublicNoteDemotes(t *testing.T) {
	tests := []struct {
		name     string
		from     model.NoteVisibility
		policies *publicNotePolicies
		want     model.NoteVisibility
	}{
		{name: "public denied", from: model.NoteVisibilityPublic,
			policies: &publicNotePolicies{mayBeFalse: true, canPublicNote: false}, want: model.NoteVisibilityHome},
		{name: "public allowed", from: model.NoteVisibilityPublic,
			policies: &publicNotePolicies{mayBeFalse: true, canPublicNote: true}, want: model.NoteVisibilityPublic},
		{name: "home is not upgraded", from: model.NoteVisibilityHome,
			policies: &publicNotePolicies{mayBeFalse: true, canPublicNote: true}, want: model.NoteVisibilityHome},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, noteRepo, _ := newProhibitedWordsUpdateResolver(t, nil)
			noteRepo.Notes["n1"].Visibility = tc.from
			r.SetRolePolicyProviderForTest(tc.policies)

			got, err := r.UpdateRemoteNote([]byte(sensitiveUpdateBody(`"content":"edited"`)), "")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Visibility)
			require.NotEmpty(t, noteRepo.UpdateFieldsCalls)
			v, written := noteRepo.UpdateFieldsCalls[0]["visibility"]
			if tc.want != tc.from {
				assert.True(t, written)
				assert.Equal(t, tc.want, v)
			} else {
				assert.False(t, written)
			}
		})
	}
}

// A conditional role that matches remote users and denies canPublicNote
// demotes their inbound public notes, through the real role service.
func TestIngestNote_CanPublicNoteFromConditionalRoleOnRemoteUser(t *testing.T) {
	r, userRepo, _, _ := newMentionLimitResolver(t)
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	metaRepo := testutil.NewMockMetaRepository()
	idGen, _ := id.NewGenerator("aidx")
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)
	roleSvc.SetUserRepo(userRepo)
	roleRepo.Roles["r-remote"] = &model.Role{
		ID:          "r-remote",
		Name:        "RemoteUsers",
		Target:      model.RoleTargetConditional,
		CondFormula: datatypes.JSON([]byte(`{"type":"isRemote"}`)),
		Policies:    datatypes.JSON([]byte(`{"canPublicNote":{"useDefault":false,"priority":0,"value":false}}`)),
	}
	r.SetRolePolicyProviderForTest(roleSvc)

	note, err := r.IngestNote(mentionNoteBody("silenced", 0))
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, model.NoteVisibilityHome, note.Visibility)
}
