package admin_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRelaySvc records calls + returns configurable results for the
// admin/relays handlers.
type fakeRelaySvc struct {
	added   []string
	removed []string
	listed  int
	retRel  *model.Relay
	retList []*model.Relay
	err     error
}

func (f *fakeRelaySvc) Add(_ context.Context, inbox string) (*model.Relay, error) {
	f.added = append(f.added, inbox)
	return f.retRel, f.err
}

func (f *fakeRelaySvc) Remove(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return f.err
}

func (f *fakeRelaySvc) List(_ context.Context) ([]*model.Relay, error) {
	f.listed++
	return f.retList, f.err
}

func TestRelaysAdd_InvalidUrl(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// upstream add.ts は inbox 未指定 / 空 / https 以外 / 非 URL を INVALID_URL で弾く。
	assert.Equal(t, http.StatusBadRequest, doPost(h.RelaysAdd, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.RelaysAdd, `{"inbox":""}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.RelaysAdd, `{"inbox":"http://r.example/inbox"}`, adminUser).Code)
	rec := doPost(h.RelaysAdd, `{"inbox":"notaurl"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_URL")
	assert.Contains(t, rec.Body.String(), "fb8c92d3-d4e5-44e7-b3d4-800d5cef8b2c")
}

func TestRelaysAdd_WithService(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	svc := &fakeRelaySvc{retRel: &model.Relay{ID: "rel1", Inbox: "https://r.example/inbox", Status: "requesting"}}
	h.SetRelayService(svc)
	rec := doPost(h.RelaysAdd, `{"inbox":"https://r.example/inbox"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"https://r.example/inbox"}, svc.added)
}

func TestRelaysAdd_ServiceError(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	svc := &fakeRelaySvc{err: errors.New("boom")}
	h.SetRelayService(svc)
	rec := doPost(h.RelaysAdd, `{"inbox":"https://r.example/inbox"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRelaysList(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.RelaysList, `{}`, adminUser).Code)
}

func TestRelaysList_WithService(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	svc := &fakeRelaySvc{retList: []*model.Relay{{ID: "a"}, {ID: "b"}}}
	h.SetRelayService(svc)
	rec := doPost(h.RelaysList, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, svc.listed)
}

func TestRelaysList_WithService_NilSlice(t *testing.T) {
	// list が nil を返しても [] を返す (本家互換)
	h, _, _, _ := newTestHandler(t)
	svc := &fakeRelaySvc{retList: nil}
	h.SetRelayService(svc)
	rec := doPost(h.RelaysList, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "[]")
}

func TestRelaysRemove(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.RelaysRemove, `{}`, adminUser).Code)
}

// relays/remove は本家と同じく inbox で引く。独自の id は読まない (#3867)。
func TestRelaysRemove_LooksUpByInbox(t *testing.T) {
	if integrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const relID, inbox = "relremove3867", "https://192.0.2.1/relay-3867/inbox"
	seed := func(t *testing.T) {
		t.Helper()
		require.NoError(t, integrationDB.Exec(`DELETE FROM relay WHERE id = ?`, relID).Error)
		require.NoError(t, integrationDB.Create(&model.Relay{ID: relID, Inbox: inbox, Status: "accepted"}).Error)
		t.Cleanup(func() { require.NoError(t, integrationDB.Exec(`DELETE FROM relay WHERE id = ?`, relID).Error) })
	}
	t.Run("with the relay service", func(t *testing.T) {
		seed(t)
		h, _, _, _ := newTestHandler(t)
		h.SetAdminDB(integrationDB)
		svc := &fakeRelaySvc{}
		h.SetRelayService(svc)
		assert.Equal(t, http.StatusNoContent, doPost(h.RelaysRemove, `{"id":"`+relID+`"}`, adminUser).Code)
		assert.Empty(t, svc.removed, "the id is not read")
		assert.Equal(t, http.StatusNoContent, doPost(h.RelaysRemove, `{"inbox":"`+inbox+`"}`, adminUser).Code)
		assert.Equal(t, []string{relID}, svc.removed)
	})
	t.Run("without the relay service", func(t *testing.T) {
		seed(t)
		h, _, _, _ := newTestHandler(t)
		h.SetAdminDB(integrationDB)
		count := func() int64 {
			var n int64
			require.NoError(t, integrationDB.Model(&model.Relay{}).Where("id = ?", relID).Count(&n).Error)
			return n
		}
		assert.Equal(t, http.StatusNoContent, doPost(h.RelaysRemove, `{"id":"`+relID+`"}`, adminUser).Code)
		assert.EqualValues(t, 1, count(), "the id is not read")
		assert.Equal(t, http.StatusNoContent, doPost(h.RelaysRemove, `{"inbox":"`+inbox+`"}`, adminUser).Code)
		assert.EqualValues(t, 0, count())
	})
}

func TestRelaysRemove_WithService_NoID(t *testing.T) {
	// inbox が無い場合はスキップして 204 を返す
	h, _, _, _ := newTestHandler(t)
	svc := &fakeRelaySvc{}
	h.SetRelayService(svc)
	rec := doPost(h.RelaysRemove, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, svc.removed)
}

// DB の障害を「見つからない・消した」の 204 に丸めない (#2792、#3867)。
func TestRelaysRemove_DBFailureIsInternalError(t *testing.T) {
	if integrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	broken := integrationDB.WithContext(ctx)
	for _, withService := range []bool{true, false} {
		h, _, _, _ := newTestHandler(t)
		h.SetAdminDB(broken)
		if withService {
			h.SetRelayService(&fakeRelaySvc{})
		}
		rec := doPost(h.RelaysRemove, `{"inbox":"https://192.0.2.1/inbox"}`, adminUser)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "relay service: %v", withService)
	}
}
