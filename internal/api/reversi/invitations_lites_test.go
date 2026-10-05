package reversi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLiteFiller marks every inviter with an instance name.
type stubLiteFiller struct{ calls int }

func (s *stubLiteFiller) FillLites(users []*model.User, lites []*entity.UserLite) {
	s.calls++
	for i, u := range users {
		name := "remote-" + u.ID
		lites[i].Instance = &entity.InstanceLite{Name: &name}
	}
}

// 本家 reversi/invitations は packMany(invitations, me) なので、招待者の
// instance と絵文字をまとめて埋める (#3330)。
func TestInvitations_FillsLitesInOneBatch(t *testing.T) {
	h, repo := newTestHandler()
	assert.False(t, h.HasLiteFiller())
	f := &stubLiteFiller{}
	h.SetLiteFiller(f)
	assert.True(t, h.HasLiteFiller())
	g := sampleGame()
	g.IsStarted = false
	repo.games["g1"] = g

	rec := post(h.Invitations, `{}`, &model.User{ID: "u2", Username: "bob"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, 1, f.calls)
	assert.Equal(t, "remote-u1", resp[0]["instance"].(map[string]any)["name"])
}
