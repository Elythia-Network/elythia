package plugintest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

func TestNoteDeliveryCapture(t *testing.T) {
	var gotName string
	definition := plugin.Definition{
		Name: "targets", APIVersion: plugin.APIVersion,
		NoteDelivery: func(ctx plugin.Context) (plugin.NoteDeliveryRegistration, error) {
			gotName = ctx.Name()
			return plugin.NoteDeliveryRegistration{
				Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
				Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) {
					return map[string]plugin.DeliveryTargets{"n1": {Mode: plugin.DeliveryTargetsInclude}}, nil
				},
			}, nil
		},
	}
	reg := plugintest.New(t).WithName("targets").NoteDelivery(definition)
	assert.Equal(t, "targets", gotName)
	got, err := reg.Targets(context.Background(), []string{"n1"})
	require.NoError(t, err)
	assert.Equal(t, plugin.DeliveryTargetsInclude, got["n1"].Mode)
}

// Each case runs in a child process because the harness fails the test.
func TestNoteDeliveryFailures(t *testing.T) {
	cases := map[string]plugin.Definition{
		"absent": {Name: "targets", APIVersion: plugin.APIVersion},
		"error": {Name: "targets", APIVersion: plugin.APIVersion, NoteDelivery: func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
			return plugin.NoteDeliveryRegistration{}, errors.New("broken")
		}},
		"incomplete": {Name: "targets", APIVersion: plugin.APIVersion, NoteDelivery: func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
			return plugin.NoteDeliveryRegistration{}, nil
		}},
	}
	want := map[string]string{
		"absent":     "NoteDelivery が宣言されていません",
		"error":      "NoteDelivery に失敗しました",
		"incomplete": "Prepare と Targets は両方とも必要です",
	}
	if name := os.Getenv("MK_PLUGINTEST_NOTE_DELIVERY_CHILD"); name != "" {
		plugintest.New(t).NoteDelivery(cases[name])
		return
	}
	for name := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNoteDeliveryFailures$")
			cmd.Env = append(os.Environ(), "MK_PLUGINTEST_NOTE_DELIVERY_CHILD="+name)
			output, err := cmd.CombinedOutput()
			require.Error(t, err)
			assert.Contains(t, string(output), want[name])
		})
	}
}
