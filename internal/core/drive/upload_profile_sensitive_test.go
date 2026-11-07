package drive_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// profileStub returns the stored profile per user (repository.ErrNotFound for
// an unknown user, like the real repository), or err for every lookup.
type profileStub struct {
	profiles map[string]*model.UserProfile
	err      error
	calls    int
}

func (p *profileStub) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	profile, ok := p.profiles[userID]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return profile, nil
}

// fixedScoreDetector reports the same score for every file and counts calls.
type fixedScoreDetector struct {
	score float64
	calls int
}

func (d *fixedScoreDetector) Detect(context.Context, []byte, string) (float64, error) {
	d.calls++
	return d.score, nil
}

// 旧来の detector の経路は画像かどうかを見ないが、本家の判定は画像と動画だけを見るので、
// 経路を本家に寄せても壊れないよう画像を上げる。
func uploadAs(t *testing.T, svc *drive.Service, user *model.User) *model.DriveFile {
	t.Helper()
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: testPNGBytes(4, 4), Name: "x.png"})
	require.NoError(t, err)
	return f
}

// プロフィールの「常にセンシティブにする」は、ローカルの利用者のアップロードに効く (#4057)。
func TestUpload_ProfileAlwaysMarkNsfw(t *testing.T) {
	host := "remote.example"
	profiles := &profileStub{profiles: map[string]*model.UserProfile{
		"on":     {UserID: "on", AlwaysMarkNsfw: true},
		"off":    {UserID: "off"},
		"remote": {UserID: "remote", AlwaysMarkNsfw: true},
		"nil":    nil,
	}}

	tests := []struct {
		name string
		user *model.User
		want bool
	}{
		{name: "local user with the setting", user: &model.User{ID: "on"}, want: true},
		{name: "local user without the setting", user: &model.User{ID: "off"}, want: false},
		// 本家と同じく、リモートの利用者には効かせない。
		{name: "remote user", user: &model.User{ID: "remote", Host: &host}, want: false},
		{name: "no profile row", user: &model.User{ID: "missing"}, want: false},
		{name: "nil profile", user: &model.User{ID: "nil"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newSvc(t)
			svc.SetUploaderProfiles(profiles)
			assert.Equal(t, tc.want, uploadAs(t, svc, tc.user).IsSensitive)
		})
	}
}

// 「常にセンシティブにする」の利用者は結果が決まっているので、判定を走らせない。
func TestUpload_ProfileAlwaysMarkNsfwSkipsDetection(t *testing.T) {
	svc, _, _ := newSvc(t)
	detector := &fixedScoreDetector{score: 0}
	svc.SetSensitiveDetection(detector, drive.SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true,
	})
	svc.SetUploaderProfiles(&profileStub{profiles: map[string]*model.UserProfile{
		"u1": {UserID: "u1", AlwaysMarkNsfw: true},
	}})
	assert.True(t, uploadAs(t, svc, &model.User{ID: "u1"}).IsSensitive)
	assert.Equal(t, 0, detector.calls)
}

// プロフィールの「自動でセンシティブにする」は、サーバーが自動で付けない設定でも、
// 判定がセンシティブとしたファイルをセンシティブにする (#4057)。
func TestUpload_ProfileAutoSensitive(t *testing.T) {
	tests := []struct {
		name          string
		autoSensitive bool
		score         float64
		want          bool
	}{
		{name: "setting on and judged sensitive", autoSensitive: true, score: 0.99, want: true},
		{name: "setting on and judged safe", autoSensitive: true, score: 0, want: false},
		{name: "setting off", autoSensitive: false, score: 0.99, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newSvc(t)
			svc.SetSensitiveDetection(&fixedScoreDetector{score: tc.score}, drive.SensitiveConfig{
				Detection: "all", Sensitivity: "medium", SetFlagAutomatically: false,
			})
			svc.SetUploaderProfiles(&profileStub{profiles: map[string]*model.UserProfile{
				"u1": {UserID: "u1", AutoSensitive: tc.autoSensitive},
			}})
			assert.Equal(t, tc.want, uploadAs(t, svc, &model.User{ID: "u1"}).IsSensitive)
		})
	}
}

// 判定の対象外 (サーバーの判定が無効) なら、「自動でセンシティブにする」でも判定しない。
func TestUpload_ProfileAutoSensitiveRespectsDetectionMode(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetSensitiveDetection(&fixedScoreDetector{score: 0.99}, drive.SensitiveConfig{
		Detection: "none", Sensitivity: "medium",
	})
	svc.SetUploaderProfiles(&profileStub{profiles: map[string]*model.UserProfile{
		"u1": {UserID: "u1", AutoSensitive: true},
	}})
	assert.False(t, uploadAs(t, svc, &model.User{ID: "u1"}).IsSensitive)
}

// DB 障害でプロフィールが読めないときは、設定を飛ばして公開せず、エラーにする。
func TestUpload_ProfileLookupErrorFails(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	dbErr := errors.New("db down")
	svc.SetUploaderProfiles(&profileStub{err: dbErr})

	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: testPNGBytes(4, 4), Name: "x.png"})
	require.ErrorIs(t, err, dbErr)
	assert.Empty(t, fileRepo.Files)
}

// 同じ中身のファイルを返す分岐はプロフィールを読まない (本家と同じ順)。
func TestUpload_ProfileNotReadForDuplicate(t *testing.T) {
	svc, _, _ := newSvc(t)
	profiles := &profileStub{profiles: map[string]*model.UserProfile{"u1": {UserID: "u1"}}}
	svc.SetUploaderProfiles(profiles)
	user := &model.User{ID: "u1"}

	first := uploadAs(t, svc, user)
	second := uploadAs(t, svc, user)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, 1, profiles.calls)
}
