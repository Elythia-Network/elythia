package i

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"

	"github.com/elythia-network/elythia/internal/testutil"
)

// #2682: 起動時の配線検査が見る述語。両側を固定する (#2682 review L-7)。
// signin と /api/i で同じ guard を共有するので、両方の述語が要る。
func TestHandler_HasTOTPReplayGuard(t *testing.T) {
	assert.False(t, (&Handler{}).HasTOTPReplayGuard(), "未配線なら false")

	h := &Handler{}
	h.SetTOTPReplayGuard(stubReplayGuard{})
	assert.True(t, h.HasTOTPReplayGuard(), "配線したら true")
}

func TestHandler_HasAuthInvalidator(t *testing.T) {
	assert.False(t, (&Handler{}).HasAuthInvalidator(), "未配線なら false")

	h := &Handler{}
	h.SetAuthInvalidator(&stubTokenInvalidator{})
	assert.True(t, h.HasAuthInvalidator(), "配線したら true")
}

// **HasAuthInvalidator では代替にならない。** RevokeToken は repo が nil
// だと invalidator を引く前に 204 で返るので、cache 側だけ配線されていても
// 失効そのものが起きない (#2682 review H-1)。
func TestHandler_HasAccessTokenRepo(t *testing.T) {
	assert.False(t, (&Handler{}).HasAccessTokenRepo(), "未配線なら false")

	h := &Handler{}
	h.SetAccessTokenRepo(testutil.NewMockAccessTokenRepository())
	assert.True(t, h.HasAccessTokenRepo(), "配線したら true")

	// invalidator だけ配線しても repo の述語は満たされない。
	onlyInvalidator := &Handler{}
	onlyInvalidator.SetAuthInvalidator(&stubTokenInvalidator{})
	assert.False(t, onlyInvalidator.HasAccessTokenRepo(),
		"authInvalidator は accessTokenRepo の代わりにならない")
}

type stubReplayGuard struct{}

func (stubReplayGuard) MarkUsed(context.Context, string, string) (bool, error) {
	return true, nil
}

// #2708: 1 つの nil で 3 つの gate が外れる (alwaysMarkNsfw の自己解除防止 /
// wordMuteLimit / canUpdateBioMedia)。
func TestHandler_HasRoleProvider(t *testing.T) {
	assert.False(t, (&Handler{}).HasRoleProvider(), "未配線なら false")

	h := &Handler{}
	h.SetRoleProvider(stubWiringRoleProvider{})
	assert.True(t, h.HasRoleProvider(), "配線したら true")
}

type stubWiringRoleProvider struct{}

func (stubWiringRoleProvider) IsAdministrator(string) bool { return false }
func (stubWiringRoleProvider) IsModerator(string) bool     { return false }
func (stubWiringRoleProvider) IsSilenced(string) bool      { return false }
func (stubWiringRoleProvider) GetUserRoles(string) ([]*model.Role, error) {
	return nil, nil
}
func (stubWiringRoleProvider) GetUserPolicies(string) map[string]any { return nil }
func (stubWiringRoleProvider) HasRolePolicy(string, string) bool     { return true }
