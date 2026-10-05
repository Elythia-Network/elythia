package channels

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blocked-host filter (セキュリティ監査 2026-09-21)
//
// チャンネルは連合先からも投稿されるのに、`blockedHosts` に入れても既存の
// ノートがタイムラインに出続けていた。upstream は
// generateBlockedHostQueryForNote を全一覧経路に通す。

func TestTimeline_DropsBlockedHostNotes(t *testing.T) {
	h, repo, _, noteRepo := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	bad, good := "bad.example", "good.example"
	noteRepo.Notes["n_bad"] = &model.Note{ID: "n_bad", ChannelID: &cid, UserID: "u_bad", Visibility: model.NoteVisibilityPublic, UserHost: &bad}
	noteRepo.Notes["n_ok"] = &model.Note{ID: "n_ok", ChannelID: &cid, UserID: "u_ok", Visibility: model.NoteVisibilityPublic, UserHost: &good}
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"bad.example"}}
	h.SetMetaRepo(meta)

	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Timeline(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := map[string]bool{}
	for _, n := range out {
		ids[n["id"].(string)] = true
	}
	assert.False(t, ids["n_bad"], "ブロック済みインスタンスのノートが channels/timeline に出ている")
	assert.True(t, ids["n_ok"], "ブロックしていない相手のノートは残ること")
}

// **meta が読めないときは 500 に倒す** (#1544 と同じ fail-closed)。
func TestTimeline_BlockedHostFetchErrorIsFatal(t *testing.T) {
	h, repo, _, noteRepo := newHandler(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	cid := "c1"
	good := "good.example"
	noteRepo.Notes["n_ok"] = &model.Note{ID: "n_ok", ChannelID: &cid, UserID: "u_ok", Visibility: model.NoteVisibilityPublic, UserHost: &good}
	meta := testutil.NewMockMetaRepository()
	meta.FetchErr = assert.AnError
	h.SetMetaRepo(meta)

	c, rec := newReq(t, `{"channelId":"c1"}`)
	require.NoError(t, h.Timeline(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// **述語が配線を見ていること。** 起動時の critical wiring 検査はこれを読むので、
// 常に true を返すようになると未配線を検出できなくなる。
func TestChannelsHandler_HasMetaRepo(t *testing.T) {
	h, _, _, _ := newHandler(t)
	assert.False(t, h.HasMetaRepo(), "未配線では false")
	h.SetMetaRepo(testutil.NewMockMetaRepository())
	assert.True(t, h.HasMetaRepo(), "配線後は true")
}
