package channels

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corechannel "github.com/shiroha-a/mk/internal/core/channel"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// channels/timeline の匿名 visitor への ugcVisibilityForVisitor gate
// (upstream 2026.10.0 channels/timeline.ts の generateUgcVisibilityQueryForVisitor)。
// チャンネルには remote 利用者の返信も入るので、`local` でも remote の行を落とす。
func TestTimeline_UGCVisibilityForVisitor(t *testing.T) {
	cases := []struct {
		name     string
		policy   string
		signedIn bool
		want     []string
	}{
		{name: "anon local hides remote note", policy: "local", want: []string{"n_loc"}},
		{name: "signed-in local keeps remote note", policy: "local", signedIn: true, want: []string{"n_loc", "n_rem"}},
		{name: "anon all keeps remote note", policy: "all", want: []string{"n_loc", "n_rem"}},
		{name: "anon unknown policy behaves like all", policy: "", want: []string{"n_loc", "n_rem"}},
		{name: "anon none returns nothing", policy: "none", want: []string{}},
		{name: "signed-in none keeps everything", policy: "none", signedIn: true, want: []string{"n_loc", "n_rem"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _, noteRepo := newHandler(t)
			h.SetUGCVisibilityLookup(func() string { return tc.policy })
			repo.Channels["c1"] = &model.Channel{ID: "c1"}
			cid := "c1"
			host := "remote.example"
			noteRepo.Notes["n_loc"] = &model.Note{ID: "n_loc", ChannelID: &cid, UserID: "u_loc", Visibility: model.NoteVisibilityPublic}
			noteRepo.Notes["n_rem"] = &model.Note{ID: "n_rem", ChannelID: &cid, UserID: "u_rem", UserHost: &host, Visibility: model.NoteVisibilityPublic}

			c, rec := newReq(t, `{"channelId":"c1"}`)
			if tc.signedIn {
				setUser(c, "viewer")
			}
			require.NoError(t, h.Timeline(c))
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

// upstream は NO_SUCH_CHANNEL を query より前に投げるので、`none` でも
// 存在しないチャンネルは空配列ではなくエラーになる。
func TestTimeline_UGCVisibilityNoneKeepsNoSuchChannel(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetUGCVisibilityLookup(func() string { return "none" })
	c, rec := newReq(t, `{"channelId":"missing"}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_CHANNEL")
}

// findFailChannelRepo makes FindByID fail with a non-not-found error.
type findFailChannelRepo struct {
	*testutil.MockChannelRepository
}

func (r *findFailChannelRepo) FindByID(_ string) (*model.Channel, error) {
	return nil, errors.New("boom")
}

// **DB 障害を not-found や空配列に丸めない** (#2799 と同じ)。
func TestTimeline_UGCVisibilityNoneChannelLookupErrorIs500(t *testing.T) {
	repo := &findFailChannelRepo{MockChannelRepository: testutil.NewMockChannelRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := corechannel.NewService(repo, testutil.NewMockChannelFollowingRepository(), testutil.NewMockNoteRepository(), idGen)
	h := NewHandler(svc, idGen)
	h.SetUGCVisibilityLookup(func() string { return "none" })
	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// 起動時の配線検査が見る述語。未配線で false、配線したら true。
func TestHandler_HasUGCVisibility(t *testing.T) {
	h := &Handler{}
	assert.False(t, h.HasUGCVisibility(), "未配線なら false")
	assert.Equal(t, "", h.ugcVisibilityNow(), "未配線なら空 (= all 扱い)")
	h.SetUGCVisibilityLookup(func() string { return "local" })
	assert.True(t, h.HasUGCVisibility(), "配線したら true")
	assert.False(t, (&Handler{metaRepo: testutil.NewMockMetaRepository()}).HasUGCVisibility(), "他の配線では true にならない")
}
