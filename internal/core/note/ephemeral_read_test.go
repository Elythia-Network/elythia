package note_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEphemeralReader serves relay-delivered notes that live only in Redis.
type stubEphemeralReader struct {
	notes    map[string]*model.Note
	touched  []string
	getErr   error
	touchErr error
}

func (s *stubEphemeralReader) GetNote(_ context.Context, id string) (*model.Note, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.notes[id], nil
}

func (s *stubEphemeralReader) Touch(_ context.Context, n *model.Note) error {
	s.touched = append(s.touched, n.ID)
	return s.touchErr
}

// 閲覧では DB に無いノートを Redis から返す。**materialize はしない。**
// リンクを踏まれるたびに永続化されると DB を膨らませない目的が崩れる。
func TestShowForAPI_ReadsEphemeralWithoutMaterializing(t *testing.T) {
	svc, repo, _ := newQueryService(t)
	host := "remote.example"
	eph := &stubEphemeralReader{notes: map[string]*model.Note{
		"eph1": {ID: "eph1", UserID: "ra", UserHost: &host, Visibility: model.NoteVisibilityPublic},
	}}
	svc.SetEphemeralReader(eph)

	got, err := svc.ShowForAPI("eph1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "eph1", got.ID)
	assert.Empty(t, repo.Notes, "閲覧では DB 行を作らないこと")
}

// 読み取りのたびに TTL を打ち直す。閲覧で materialize しないため、詳細を
// 開いた直後に期限切れでリアクションできなくなる穴を塞ぐ。
func TestShowForAPI_TouchesTTL(t *testing.T) {
	svc, _, _ := newQueryService(t)
	host := "remote.example"
	eph := &stubEphemeralReader{notes: map[string]*model.Note{
		"eph1": {ID: "eph1", UserID: "ra", UserHost: &host},
	}}
	svc.SetEphemeralReader(eph)

	_, err := svc.ShowForAPI("eph1")
	require.NoError(t, err)
	assert.Equal(t, []string{"eph1"}, eph.touched)
}

// DB にあるノートでは Redis を引かない (ホットパスに追加コストを載せない)。
func TestShowForAPI_SkipsEphemeralForDBNote(t *testing.T) {
	svc, repo, _ := newQueryService(t)
	repo.Notes["db1"] = &model.Note{ID: "db1", UserID: "u1"}
	eph := &stubEphemeralReader{notes: map[string]*model.Note{}}
	svc.SetEphemeralReader(eph)

	got, err := svc.ShowForAPI("db1")
	require.NoError(t, err)
	assert.Equal(t, "db1", got.ID)
	assert.Empty(t, eph.touched, "DB にあるなら Redis を引かない")
}

func TestShowForAPI_MissingEverywhere(t *testing.T) {
	svc, _, _ := newQueryService(t)
	svc.SetEphemeralReader(&stubEphemeralReader{notes: map[string]*model.Note{}})

	_, err := svc.ShowForAPI("ghost")
	assert.Error(t, err)
}

// Redis 障害では従来どおり NotFound に倒れる。
func TestShowForAPI_EphemeralErrorDegrades(t *testing.T) {
	svc, _, _ := newQueryService(t)
	svc.SetEphemeralReader(&stubEphemeralReader{getErr: errors.New("redis down")})

	_, err := svc.ShowForAPI("eph1")
	assert.Error(t, err)
}

// Touch 失敗は読み取り自体を壊さない (ベストエフォート)。
func TestShowForAPI_TouchFailureIsBestEffort(t *testing.T) {
	svc, _, _ := newQueryService(t)
	host := "remote.example"
	svc.SetEphemeralReader(&stubEphemeralReader{
		notes:    map[string]*model.Note{"eph1": {ID: "eph1", UserHost: &host}},
		touchErr: errors.New("expire failed"),
	})

	got, err := svc.ShowForAPI("eph1")
	require.NoError(t, err)
	assert.Equal(t, "eph1", got.ID)
}

func TestShowForAPI_NoEphemeralReaderWired(t *testing.T) {
	svc, _, _ := newQueryService(t)
	_, err := svc.ShowForAPI("ghost")
	assert.Error(t, err)
}

func TestShowForAPIOnPrimary_FallsBackToEphemeral(t *testing.T) {
	svc, _, _ := newQueryService(t)
	host := "remote.example"
	svc.SetEphemeralReader(&stubEphemeralReader{notes: map[string]*model.Note{
		"eph1": {ID: "eph1", UserID: "ra", UserHost: &host, Visibility: model.NoteVisibilityPublic},
	}})

	got, err := svc.ShowForAPIOnPrimary("eph1")
	require.NoError(t, err)
	assert.Equal(t, "eph1", got.ID)
}

func TestShowForAPIOnPrimary_MissingEverywhere(t *testing.T) {
	svc, _, _ := newQueryService(t)
	svc.SetEphemeralReader(&stubEphemeralReader{notes: map[string]*model.Note{}})

	_, err := svc.ShowForAPIOnPrimary("missing")
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}
