package clips

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// clips/notes の匿名 visitor への ugcVisibilityForVisitor gate。upstream
// clips/notes.ts は generateVisibilityQuery だけを通すので、効くのは `none`
// (匿名 → 1=0) だけで、`local` では remote のノートも残る。
func TestNotes_UGCVisibilityForVisitor(t *testing.T) {
	cases := []struct {
		name     string
		policy   string
		signedIn bool
		want     []string
	}{
		{name: "anon none returns nothing", policy: "none", want: []string{}},
		{name: "signed-in none keeps notes", policy: "none", signedIn: true, want: []string{"n_loc", "n_rem"}},
		{name: "anon all keeps notes", policy: "all", want: []string{"n_loc", "n_rem"}},
		// upstream の TODO どおり、clip に入った remote のノートは `local` でも隠さない。
		{name: "anon local keeps remote note", policy: "local", want: []string{"n_loc", "n_rem"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, clipNoteRepo, notes := newHandler(t)
			h.SetUGCVisibilityLookup(func() string { return tc.policy })
			repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
			host := "remote.example"
			clipNoteRepo.Entries["cn_loc"] = &model.ClipNote{ID: "cn_loc", ClipID: "c1", NoteID: "n_loc"}
			clipNoteRepo.Entries["cn_rem"] = &model.ClipNote{ID: "cn_rem", ClipID: "c1", NoteID: "n_rem"}
			notes.Notes["n_loc"] = &model.Note{ID: "n_loc", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice"}}
			notes.Notes["n_rem"] = &model.Note{ID: "n_rem", UserID: "bob", UserHost: &host, Visibility: "public", User: &model.User{ID: "bob", Host: &host}}

			c, rec := newReq(t, `{"clipId":"c1"}`)
			if tc.signedIn {
				setUser(c, "viewer")
			}
			require.NoError(t, h.Notes(c))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var out []map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			ids := make([]string, 0, len(out))
			for _, n := range out {
				ids = append(ids, n["id"].(string))
			}
			sort.Strings(ids)
			assert.Equal(t, tc.want, ids)
		})
	}
}

// upstream は NO_SUCH_CLIP を query より前に投げるので、`none` でも非公開 clip は
// 空配列ではなくエラーになる。
func TestNotes_UGCVisibilityNoneKeepsNoSuchClip(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	h.SetUGCVisibilityLookup(func() string { return "none" })
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: false}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CLIP")
}

// 起動時の配線検査が見る述語。他の述語と取り違えていないことも見る。
func TestHandler_HasUGCVisibility(t *testing.T) {
	assert.False(t, (&Handler{}).HasUGCVisibility(), "未配線なら false")

	h := &Handler{}
	h.SetUGCVisibilityLookup(func() string { return "local" })
	assert.True(t, h.HasUGCVisibility(), "配線したら true")
	assert.False(t, h.HasMetaRepo(), "他の述語は満たされないこと")

	other := &Handler{}
	other.SetMetaRepo(testutil.NewMockMetaRepository())
	assert.False(t, other.HasUGCVisibility())
}
