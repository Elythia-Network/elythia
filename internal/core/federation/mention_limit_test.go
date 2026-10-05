package federation_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// fakeMentionPolicies returns a fixed mentionLimit for one user and records the
// user IDs it was asked about.
type fakeMentionPolicies struct {
	userID string
	limit  any
	asked  []string
}

func (f *fakeMentionPolicies) GetUserPolicies(userID string) map[string]any {
	f.asked = append(f.asked, userID)
	if userID != f.userID {
		return map[string]any{}
	}
	return map[string]any{"mentionLimit": f.limit}
}

// mentionNoteBody renders a public remote note by alice with n Mention tags.
func mentionNoteBody(noteID string, n int) []byte {
	tags := make([]string, 0, n)
	for i := 0; i < n; i++ {
		tags = append(tags, fmt.Sprintf(`{"type": "Mention", "href": "https://example.com/users/local%d"}`, i))
	}
	return []byte(fmt.Sprintf(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/%s",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hi",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"cc": [],
		"tag": [%s]
	}`, noteID, strings.Join(tags, ",")))
}

func newMentionLimitResolver(t *testing.T) (*federation.Resolver, *testutil.MockUserRepository, *testutil.MockNoteRepository, *model.User) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	plantNumberedLocalUsers(repo, corenote.DefaultMentionLimit+5)
	author, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	return r, repo, noteRepo, author
}

// #3330: upstream NoteCreateService.create compares the mention count with
// `roleService.getUserPolicies(user.id).mentionLimit` for remote authors too,
// so the remote author's role policy decides the limit of an inbound note.
func TestIngestNote_MentionLimitFollowsAuthorRolePolicy(t *testing.T) {
	t.Run("custom limit from the author's policies", func(t *testing.T) {
		r, _, _, author := newMentionLimitResolver(t)
		p := &fakeMentionPolicies{userID: author.ID, limit: 2}
		r.SetRolePolicyProvider(p)

		_, err := r.IngestNote(mentionNoteBody("over", 3))
		require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
		assert.Contains(t, p.asked, author.ID, "投稿者 (リモート) の policy を引く")

		got, err := r.IngestNote(mentionNoteBody("at", 2))
		require.NoError(t, err, "上限ちょうどは通る")
		assert.Len(t, []string(got.Mentions), 2)
	})

	t.Run("limit raised above the default", func(t *testing.T) {
		r, _, _, author := newMentionLimitResolver(t)
		r.SetRolePolicyProvider(&fakeMentionPolicies{userID: author.ID, limit: corenote.DefaultMentionLimit + 5})
		_, err := r.IngestNote(mentionNoteBody("raised", corenote.DefaultMentionLimit+5))
		require.NoError(t, err, "ロールで上げた上限まではリモートでも通る")
	})

	t.Run("limit zero rejects any mention", func(t *testing.T) {
		r, _, _, author := newMentionLimitResolver(t)
		r.SetRolePolicyProvider(&fakeMentionPolicies{userID: author.ID, limit: 0})
		_, err := r.IngestNote(mentionNoteBody("zero-one", 1))
		require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
		_, err = r.IngestNote(mentionNoteBody("zero-none", 0))
		require.NoError(t, err, "メンションの無い note は通す (本家の count > 0 条件)")
	})

	t.Run("unwired provider keeps the default", func(t *testing.T) {
		r, _, _, _ := newMentionLimitResolver(t)
		_, err := r.IngestNote(mentionNoteBody("default-over", corenote.DefaultMentionLimit+1))
		require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
	})
}

// #3330 review: upstream evaluates `count > 0 && count > mentionLimit`, so a
// note without mentions never reads the author's policies. Reading them for
// every inbound note would grow role.Service's per-user cache with every
// remote author.
func TestIngestNote_NoMentionsDoesNotLookUpPolicies(t *testing.T) {
	r, _, noteRepo, author := newMentionLimitResolver(t)
	p := &fakeMentionPolicies{userID: author.ID, limit: 2}
	r.SetRolePolicyProvider(p)

	_, err := r.IngestNote(mentionNoteBody("no-mentions", 0))
	require.NoError(t, err)
	assert.Empty(t, p.asked, "メンションの無い note では policy を引かない")

	host := "remote.example"
	uri := "https://remote.example/notes/no-mentions-edit"
	original := "original"
	noteRepo.Notes["n-no-mentions-edit"] = &model.Note{
		ID: "n-no-mentions-edit", UserID: author.ID, UserHost: &host, URI: &uri, Text: &original,
		Mentions: model.StringArray{},
	}
	_, err = r.UpdateRemoteNote(mentionNoteBody("no-mentions-edit", 0), "")
	require.NoError(t, err)
	assert.Empty(t, p.asked, "Update でも引かない")

	_, err = r.IngestNote(mentionNoteBody("one-mention", 1))
	require.NoError(t, err)
	assert.Equal(t, []string{author.ID}, p.asked, "メンションがあれば投稿者の policy を 1 回引く")
}

func TestExceedsRemoteMentionLimit_LimitIsLazy(t *testing.T) {
	called := 0
	limit := func() int { called++; return 0 }
	assert.False(t, federation.ExceedsRemoteMentionLimitFunc(&model.Note{UserID: "a"}, nil, nil, limit))
	assert.Zero(t, called, "数が 0 なら上限を引かない")
	assert.True(t, federation.ExceedsRemoteMentionLimitFunc(&model.Note{UserID: "a"}, []string{"m"}, nil, limit))
	assert.Equal(t, 1, called)
}

// #3330: a conditional role whose formula matches remote users (isRemote)
// applies its mentionLimit to inbound notes, through the real role service.
func TestIngestNote_MentionLimitFromConditionalRoleOnRemoteUser(t *testing.T) {
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
		Policies:    datatypes.JSON([]byte(`{"mentionLimit":{"useDefault":false,"priority":0,"value":1}}`)),
	}
	r.SetRolePolicyProvider(roleSvc)

	_, err := r.IngestNote(mentionNoteBody("cond-over", 2))
	require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
	_, err = r.IngestNote(mentionNoteBody("cond-at", 1))
	require.NoError(t, err)
}

// #3330: the Update path applies the same per-author limit as creation.
func TestUpdateRemoteNote_MentionLimitFollowsAuthorRolePolicy(t *testing.T) {
	r, _, noteRepo, author := newMentionLimitResolver(t)
	r.SetRolePolicyProvider(&fakeMentionPolicies{userID: author.ID, limit: 1})
	host := "remote.example"
	uri := "https://remote.example/notes/edit-policy"
	original := "original"
	noteRepo.Notes["n-edit-policy"] = &model.Note{
		ID: "n-edit-policy", UserID: author.ID, UserHost: &host, URI: &uri, Text: &original,
		Mentions: model.StringArray{},
	}
	got, err := r.UpdateRemoteNote(mentionNoteBody("edit-policy", 2), "")
	require.NoError(t, err)
	assert.Equal(t, "original", *got.Text, "投稿者の上限 (1) を超える Update は捨てる")

	got, err = r.UpdateRemoteNote(mentionNoteBody("edit-policy", 1), "")
	require.NoError(t, err)
	assert.Equal(t, "hi", *got.Text)
}
