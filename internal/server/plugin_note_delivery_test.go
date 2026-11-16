package server

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/plugin"
)

func noteDeliveryDef(name string, fn func(plugin.Context) (plugin.NoteDeliveryRegistration, error)) plugin.Definition {
	return plugin.Definition{Name: name, Version: "1", APIVersion: plugin.APIVersion, NoteDelivery: fn}
}

func validNoteDelivery(targets map[string]plugin.DeliveryTargets) plugin.NoteDeliveryRegistration {
	return plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return targets, nil },
	}
}

// NoteDelivery is registered in every role (#4199): the API
// process stores the targets and the queue process delivers poll updates.
func TestSetupPlugins_NoteDeliveryRegisteredInEveryRole(t *testing.T) {
	for _, role := range []config.ProcessRole{config.RoleBoth, config.RoleServer, config.RoleQueue} {
		t.Run(string(role), func(t *testing.T) {
			s, api := newPluginTestServer(role)
			s.noteDelivery = notedelivery.New()
			def := noteDeliveryDef("targets", func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
				return validNoteDelivery(map[string]plugin.DeliveryTargets{"n1": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}}}), nil
			})
			require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
			rule := s.noteDelivery.Rule(context.Background(), "n1")
			assert.True(t, rule.Restricted())
			assert.False(t, rule.Allows("a.example"))
		})
	}
}

func TestSetupPlugins_NoteDeliveryDisabledPluginIsNotRegistered(t *testing.T) {
	s, api := newPluginTestServer(config.RoleBoth)
	s.noteDelivery = notedelivery.New()
	s.config.Plugins = map[string]map[string]any{"targets": {"enabled": false}}
	called := false
	def := noteDeliveryDef("targets", func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
		called = true
		return validNoteDelivery(nil), nil
	})
	require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
	assert.False(t, called)
	assert.True(t, s.noteDelivery.Empty())
}

// A plugin meant to narrow delivery that fails to register must stop startup:
// running without it would federate restricted notes everywhere.
func TestSetupPlugins_NoteDeliveryFailureStopsStartup(t *testing.T) {
	tests := []struct {
		name string
		fn   func(plugin.Context) (plugin.NoteDeliveryRegistration, error)
	}{
		{name: "error", fn: func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
			return plugin.NoteDeliveryRegistration{}, errors.New("broken")
		}},
		{name: "panic", fn: func(plugin.Context) (plugin.NoteDeliveryRegistration, error) { panic("boom") }},
		{name: "missing callbacks", fn: func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
			return plugin.NoteDeliveryRegistration{}, nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, api := newPluginTestServer(config.RoleBoth)
			s.noteDelivery = notedelivery.New()
			err := s.setupPlugins(api, []plugin.Definition{noteDeliveryDef("targets", tt.fn)}, noopStorage)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "配送先の絞り込みの登録に失敗")
		})
	}

	s, api := newPluginTestServer(config.RoleBoth)
	err := s.setupPlugins(api, []plugin.Definition{noteDeliveryDef("targets", func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
		return validNoteDelivery(nil), nil
	})}, noopStorage)
	require.Error(t, err, "an unwired registry must not be skipped silently")
}
