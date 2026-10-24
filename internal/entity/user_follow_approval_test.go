package entity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// MeDetailed は本家 2026.10.1 (PR 17998) の followApprovalLocalSeconds /
// followApprovalRemoteSeconds を、未設定でも null で必ず出す (本家の schema で
// optional ではないため、#3809)。Elythia 独自の followApprovalAction は、
// 未設定 (既定の request / 未知の値) なら key ごと出さない — 本家の e2e が
// MeDetailed の key の過不足を完全一致で見るため (#3466)。0 は null と区別して出す。
func TestPackMeDetailed_FollowApproval(t *testing.T) {
	u := &model.User{ID: "u1", Username: "u1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	decode := func(me MeDetailed) map[string]any {
		b, err := json.Marshal(me)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		return m
	}

	for name, profile := range map[string]*model.UserProfile{
		"no profile":     nil,
		"unset":          {UserID: "u1"},
		"unknown action": {UserID: "u1", FollowApprovalAction: "bogus"},
	} {
		m := decode(currentPacker().PackMeDetailed(u, profile))
		for _, k := range []string{"followApprovalLocalSeconds", "followApprovalRemoteSeconds"} {
			v, ok := m[k]
			assert.True(t, ok, "%s: %s は未設定でも出す", name, k)
			assert.Nil(t, v, "%s: %s", name, k)
		}
		assert.NotContains(t, m, "followApprovalAction", name)
	}
	m := decode(currentPacker().PackMeDetailed(u, &model.UserProfile{UserID: "u1", FollowApprovalAction: model.FollowApprovalActionRequest}))
	assert.NotContains(t, m, "followApprovalAction", "既定の request は出さない")

	m = decode(currentPacker().PackMeDetailed(u, &model.UserProfile{
		UserID:                      "u1",
		FollowApprovalLocalSeconds:  new(0),
		FollowApprovalRemoteSeconds: new(86400),
		FollowApprovalAction:        model.FollowApprovalActionSilentFollow,
	}))
	assert.EqualValues(t, 0, m["followApprovalLocalSeconds"], "0 は null と区別する")
	assert.EqualValues(t, 86400, m["followApprovalRemoteSeconds"])
	assert.Equal(t, "silentFollow", m["followApprovalAction"])

	// 他人に見せる UserDetailed には出さない (本家の e2e と同じ)。
	d, err := json.Marshal(currentPacker().PackUserDetailed(u, &model.UserProfile{UserID: "u1", FollowApprovalLocalSeconds: new(60)}))
	require.NoError(t, err)
	assert.NotContains(t, string(d), "followApproval")
}
