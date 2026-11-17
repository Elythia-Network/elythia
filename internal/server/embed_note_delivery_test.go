package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

func visitorDeliveryRules(t *testing.T, targets map[string]plugin.DeliveryTargets) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.NewForServer("https://local.example")
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return targets, nil },
	}))
	return r
}

// The embed is read without signing in, so a note whose conditions keep it
// from this server's own host is not embedded (#4203).
func TestEmbedNote_HiddenFromVisitorsByDeliveryRules(t *testing.T) {
	tests := []struct {
		name      string
		targets   plugin.DeliveryTargets
		wantEmbed bool
	}{
		{name: "exclude own host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example"}}, wantEmbed: false},
		{name: "exclude other host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}}, wantEmbed: true},
		{name: "include own host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"local.example"}}, wantEmbed: true},
		{name: "include other host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}}, wantEmbed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			noteRepo := testutil.NewMockNoteRepository()
			noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
			h := newEmbedHandlers(&config.Config{URL: "https://local.example"}, testutil.NewMockMetaRepository(), nil, nil, nil, nil, nil, EmbedDeps{
				NoteRepo:     noteRepo,
				PackNote:     func(*echo.Context, *model.Note) (any, error) { return map[string]string{"id": "packed-n1"}, nil },
				NoteDelivery: visitorDeliveryRules(t, map[string]plugin.DeliveryTargets{"n1": tt.targets}),
			})
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/notes/n1", nil), rec)
			c.SetPathValues(echo.PathValues{{Name: "note", Value: "n1"}})
			require.NoError(t, h.Note(c))
			assert.Equal(t, tt.wantEmbed, strings.Contains(rec.Body.String(), "packed-n1"))
		})
	}
}

// While the plugin cannot report the conditions, the note is not embedded
// (#4203).
func TestEmbedNote_PluginFailureHidesNote(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic}
	rules := notedelivery.NewForServer("https://local.example")
	require.NoError(t, rules.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) {
			return nil, errors.New("db down")
		},
	}))
	h := newEmbedHandlers(&config.Config{URL: "https://local.example"}, testutil.NewMockMetaRepository(), nil, nil, nil, nil, nil, EmbedDeps{
		NoteRepo:     noteRepo,
		PackNote:     func(*echo.Context, *model.Note) (any, error) { return map[string]string{"id": "packed-n1"}, nil },
		NoteDelivery: rules,
	})
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/embed/notes/n1", nil), rec)
	c.SetPathValues(echo.PathValues{{Name: "note", Value: "n1"}})
	require.NoError(t, h.Note(c))
	assert.NotContains(t, rec.Body.String(), "packed-n1")
}

// The feed is read without signing in, so it leaves out the notes whose
// conditions keep them from this server's own host (#4203).
func TestFeed_HiddenFromVisitorsByDeliveryRules(t *testing.T) {
	h := newFeedTestHandler([]*model.Note{
		{ID: "shown", UserID: "u1", Text: strp("shown body"), Visibility: model.NoteVisibilityPublic},
		{ID: "hidden", UserID: "u1", Text: strp("hidden body"), Visibility: model.NoteVisibilityPublic},
	})
	h.noteDelivery = visitorDeliveryRules(t, map[string]plugin.DeliveryTargets{
		"shown":  {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}},
		"hidden": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example"}},
	})
	for name, serve := range map[string]func(*echo.Context, string) error{"rss": h.RSS, "atom": h.Atom, "json": h.JSON} {
		t.Run(name, func(t *testing.T) {
			rec := doFeedReq(t, serve, "alice")
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), "shown body")
			assert.NotContains(t, rec.Body.String(), "hidden body")
			assert.NotContains(t, rec.Body.String(), "/notes/hidden")
		})
	}
}
