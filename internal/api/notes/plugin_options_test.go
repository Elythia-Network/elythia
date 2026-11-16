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

	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/model"
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
