package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

func postCreate(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	setAuthUser(c, &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))})
	require.NoError(t, h.Create(c))
	return rec
}

func registryWith(t *testing.T, prepare func(context.Context, plugin.NoteDeliveryPrepareRequest) error) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: prepare,
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return nil, nil },
	}))
	return r
}

// pluginOptions reaches the plugin with the stored note's ID (#4199).
func TestCreate_PluginOptions_PreparedWithNoteID(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	var got plugin.NoteDeliveryPrepareRequest
	h.SetNoteDeliveryRules(registryWith(t, func(_ context.Context, req plugin.NoteDeliveryPrepareRequest) error {
		got = req
		return nil
	}))

	rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{"mode":"include","hosts":["a.example"]}}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		CreatedNote struct {
			ID string `json:"id"`
		} `json:"createdNote"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, resp.CreatedNote.ID, got.NoteID)
	assert.Equal(t, "user1", got.UserID)
	assert.JSONEq(t, `{"mode":"include","hosts":["a.example"]}`, string(got.Option))
	assert.Len(t, noteRepo.Notes, 1)
}

func TestCreate_PluginOptions_Rejected(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		prepare  error
		wantCode int
	}{
		{name: "unknown plugin", body: `{"text":"hi","pluginOptions":{"nope":{}}}`, wantCode: http.StatusBadRequest},
		{name: "bad option", body: `{"text":"hi","pluginOptions":{"targets":{}}}`, prepare: fmt.Errorf("%w: empty", plugin.ErrBadNoteOption), wantCode: http.StatusBadRequest},
		{name: "plugin failure", body: `{"text":"hi","pluginOptions":{"targets":{}}}`, prepare: errors.New("db down"), wantCode: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, noteRepo := newTestHandler(t)
			h.SetNoteDeliveryRules(registryWith(t, func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return tt.prepare }))
			rec := postCreate(t, h, tt.body)
			assert.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
			assert.Empty(t, noteRepo.Notes, "no note may be created")
			assert.NotContains(t, rec.Body.String(), "db down", "the plugin's error must not leak")
		})
	}
}

// Without a NoteDelivery plugin, any pluginOptions entry is rejected rather
// than silently ignored: the client asked for a restriction nobody enforces.
func TestCreate_PluginOptions_NoRegistry(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{"mode":"include","hosts":[]}}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, noteRepo.Notes)

	rec = postCreate(t, h, `{"text":"hi","pluginOptions":{}}`)
	assert.Equal(t, http.StatusOK, rec.Code, "an empty object asks for nothing")
}

// insertFailingNoteRepo fails to store notes, as the database would.
type insertFailingNoteRepo struct {
	*testutil.MockNoteRepository
}

func (*insertFailingNoteRepo) Create(*model.Note) error { return errors.New("insert failed") }

// insertFailingPollRepo fails after the note row is written.
type insertFailingPollRepo struct {
	*testutil.MockPollRepository
}

func (*insertFailingPollRepo) Create(*model.Poll) error { return errors.New("poll insert failed") }

// forgetRegistry returns a registry whose plugin records the IDs it was told
// to forget, failing Prepare with prepareErr.
func forgetRegistry(t *testing.T, prepareErr error) (*notedelivery.Registry, *[]string) {
	t.Helper()
	var forgot []string
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return prepareErr },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return nil, nil },
		ForgetNotes: func(_ context.Context, ids []string) error {
			forgot = append(forgot, ids...)
			return nil
		},
	}))
	return r, &forgot
}

func newHandlerWithRepos(t *testing.T, noteRepo repository.NoteRepository, pollRepo repository.PollRepository) *Handler {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	createSvc := corenote.NewCreateService(noteRepo, pollRepo, idGen, nil, nil, nil, nil, nil, nil, nil)
	return newHandlerNilDeps(noteRepo, createSvc, corenote.NewDeleteService(noteRepo), corenote.NewQueryService(noteRepo, nil), nil, nil, nil, nil, idGen)
}

// The rows Prepare wrote are dropped when the note is not stored (#4202).
func TestCreate_PluginOptions_ForgetWhenNotStored(t *testing.T) {
	t.Run("insert failed", func(t *testing.T) {
		h := newHandlerWithRepos(t, &insertFailingNoteRepo{testutil.NewMockNoteRepository()}, testutil.NewMockPollRepository())
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Len(t, *forgot, 1)
	})
	t.Run("prepare failed", func(t *testing.T) {
		h, noteRepo := newTestHandler(t)
		r, forgot := forgetRegistry(t, fmt.Errorf("%w: empty", plugin.ErrBadNoteOption))
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Empty(t, noteRepo.Notes)
		assert.Len(t, *forgot, 1, "another plugin may have stored rows before this one refused")
	})
}

// A note whose row was written before the failure can still be fetched over
// ActivityPub, so its conditions must stay (#4202).
func TestCreate_PluginOptions_KeepWhenStored(t *testing.T) {
	t.Run("failed after insert", func(t *testing.T) {
		noteRepo := testutil.NewMockNoteRepository()
		h := newHandlerWithRepos(t, noteRepo, &insertFailingPollRepo{testutil.NewMockPollRepository()})
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","poll":{"choices":["a","b"]},"pluginOptions":{"targets":{}}}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Len(t, noteRepo.Notes, 1, "the note row was written")
		assert.Empty(t, *forgot)
	})
	t.Run("cannot tell", func(t *testing.T) {
		noteRepo := testutil.NewMockNoteRepository()
		h := newHandlerWithRepos(t, &insertFailingNoteRepo{noteRepo}, testutil.NewMockPollRepository())
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		noteRepo.FindErr = errors.New("db down")
		postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		assert.Empty(t, *forgot, "a lookup failure is not proof that the note is gone")
	})
	t.Run("federated note", func(t *testing.T) {
		h, _ := newTestHandler(t)
		r, forgot := forgetRegistry(t, nil)
		h.SetNoteDeliveryRules(r)
		rec := postCreate(t, h, `{"text":"hi","pluginOptions":{"targets":{}}}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Empty(t, *forgot)
	})
}

// A local-only note never federates, so its conditions are not needed.
func TestCreate_PluginOptions_ForgetLocalOnly(t *testing.T) {
	h, noteRepo := newTestHandler(t)
	r, forgot := forgetRegistry(t, nil)
	h.SetNoteDeliveryRules(r)
	rec := postCreate(t, h, `{"text":"hi","localOnly":true,"pluginOptions":{"targets":{}}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, noteRepo.Notes, 1)
	for id := range noteRepo.Notes {
		assert.Equal(t, []string{id}, *forgot)
	}
}

// Without pluginOptions nothing was prepared, so nothing is forgotten.
func TestCreate_PluginOptions_NothingPreparedNothingForgotten(t *testing.T) {
	h, _ := newTestHandler(t)
	r, forgot := forgetRegistry(t, nil)
	h.SetNoteDeliveryRules(r)
	rec := postCreate(t, h, `{"text":"hi","localOnly":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, *forgot)
}
