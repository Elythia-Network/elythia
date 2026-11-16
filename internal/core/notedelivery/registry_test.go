package notedelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
)

type fakePlugin struct {
	prepared []plugin.NoteDeliveryPrepareRequest
	targets  map[string]plugin.DeliveryTargets
	calls    [][]string

	prepareErr error
	targetsErr error
	panicOn    string
}

func (f *fakePlugin) registration() plugin.NoteDeliveryRegistration {
	return plugin.NoteDeliveryRegistration{
		Prepare: func(_ context.Context, req plugin.NoteDeliveryPrepareRequest) error {
			if f.panicOn == "prepare" {
				panic("boom")
			}
			if f.prepareErr != nil {
				return f.prepareErr
			}
			f.prepared = append(f.prepared, req)
			return nil
		},
		Targets: func(ctx context.Context, ids []string) (map[string]plugin.DeliveryTargets, error) {
			if f.panicOn == "targets" {
				panic("boom")
			}
			if _, ok := ctx.Deadline(); !ok {
				return nil, errors.New("no deadline")
			}
			f.calls = append(f.calls, ids)
			if f.targetsErr != nil {
				return nil, f.targetsErr
			}
			return f.targets, nil
		},
	}
}

func TestAdd_Validates(t *testing.T) {
	r := New()
	f := &fakePlugin{}
	require.Error(t, r.Add("", f.registration()))
	require.Error(t, r.Add("a", plugin.NoteDeliveryRegistration{Targets: f.registration().Targets}))
	require.Error(t, r.Add("a", plugin.NoteDeliveryRegistration{Prepare: f.registration().Prepare}))
	require.NoError(t, r.Add("a", f.registration()))
	require.Error(t, r.Add("a", f.registration()), "a duplicate name must be rejected")
	assert.False(t, r.Empty())
}

func TestNilRegistry_IsEmptyAndUnrestricted(t *testing.T) {
	var r *Registry
	assert.True(t, r.Empty())
	assert.Equal(t, Rule{}, r.Rule(context.Background(), "n1"))
	require.NoError(t, r.Prepare(context.Background(), "n1", "u1", nil))
	err := r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{"x": json.RawMessage(`{}`)})
	require.ErrorIs(t, err, ErrUnknownPlugin)
}

func TestPrepare_PassesOptionAndIDs(t *testing.T) {
	r := New()
	f := &fakePlugin{}
	require.NoError(t, r.Add("targets", f.registration()))

	err := r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{"targets": json.RawMessage(`{"mode":"include"}`)})
	require.NoError(t, err)
	require.Len(t, f.prepared, 1)
	assert.Equal(t, "n1", f.prepared[0].NoteID)
	assert.Equal(t, "u1", f.prepared[0].UserID)
	assert.JSONEq(t, `{"mode":"include"}`, string(f.prepared[0].Option))
}

func TestPrepare_SkipsNull(t *testing.T) {
	r := New()
	f := &fakePlugin{}
	require.NoError(t, r.Add("targets", f.registration()))
	require.NoError(t, r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{"targets": json.RawMessage(`null`)}))
	require.NoError(t, r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{"targets": nil}))
	assert.Empty(t, f.prepared)
}

func TestPrepare_UnknownNameCallsNoPlugin(t *testing.T) {
	r := New()
	f := &fakePlugin{}
	require.NoError(t, r.Add("targets", f.registration()))
	err := r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{
		"targets": json.RawMessage(`{}`),
		"zzz":     json.RawMessage(`{}`),
	})
	require.ErrorIs(t, err, ErrUnknownPlugin)
	assert.Empty(t, f.prepared, "no plugin may store anything when one name is unknown")
}

func TestPrepare_Errors(t *testing.T) {
	tests := []struct {
		name    string
		plugin  *fakePlugin
		wantBad bool
	}{
		{name: "bad option", plugin: &fakePlugin{prepareErr: fmt.Errorf("%w: empty", plugin.ErrBadNoteOption)}, wantBad: true},
		{name: "storage failure", plugin: &fakePlugin{prepareErr: errors.New("db down")}},
		{name: "panic", plugin: &fakePlugin{panicOn: "prepare"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			require.NoError(t, r.Add("targets", tt.plugin.registration()))
			err := r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{"targets": json.RawMessage(`{}`)})
			require.Error(t, err)
			assert.Equal(t, tt.wantBad, errors.Is(err, ErrBadOption))
		})
	}
}

func TestRules_NoPluginsIsUnrestricted(t *testing.T) {
	rules := New().Rules(context.Background(), []string{"n1", "n2"})
	require.Len(t, rules, 2)
	for _, rule := range rules {
		assert.False(t, rule.Restricted())
		assert.True(t, rule.Allows("any.example"))
		assert.True(t, rule.Allows(""))
	}
}

func TestRules_AppliesTargets(t *testing.T) {
	r := New()
	f := &fakePlugin{targets: map[string]plugin.DeliveryTargets{
		"n1": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}},
		"zz": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}},
	}}
	require.NoError(t, r.Add("targets", f.registration()))

	rules := r.Rules(context.Background(), []string{"n1", "n2"})
	require.Len(t, rules, 2, "IDs the caller did not ask for must not appear")

	assert.True(t, rules["n1"].Restricted())
	assert.True(t, rules["n1"].Allows("a.example"))
	assert.False(t, rules["n1"].Allows("b.example"))
	assert.False(t, rules["n1"].Allows(""), "an unknown requester must not see an include note")

	assert.False(t, rules["n2"].Restricted())
	assert.True(t, rules["n2"].Allows("b.example"))
}

func TestRules_IntersectsPlugins(t *testing.T) {
	r := New()
	include := &fakePlugin{targets: map[string]plugin.DeliveryTargets{
		"n1": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example", "b.example"}},
	}}
	exclude := &fakePlugin{targets: map[string]plugin.DeliveryTargets{
		"n1": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"b.example"}},
	}}
	require.NoError(t, r.Add("one", include.registration()))
	require.NoError(t, r.Add("two", exclude.registration()))

	rule := r.Rule(context.Background(), "n1")
	assert.True(t, rule.Allows("a.example"))
	assert.False(t, rule.Allows("b.example"))
	assert.False(t, rule.Allows("c.example"))
}

func TestRules_FailureDeniesEverything(t *testing.T) {
	tests := []struct {
		name   string
		plugin *fakePlugin
	}{
		{name: "error", plugin: &fakePlugin{targetsErr: errors.New("db down")}},
		{name: "panic", plugin: &fakePlugin{panicOn: "targets"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			healthy := &fakePlugin{targets: map[string]plugin.DeliveryTargets{
				"n1": {Mode: plugin.DeliveryTargetsExclude},
			}}
			require.NoError(t, r.Add("a-healthy", healthy.registration()))
			require.NoError(t, r.Add("b-broken", tt.plugin.registration()))
			rules := r.Rules(context.Background(), []string{"n1", "n2"})
			for _, id := range []string{"n1", "n2"} {
				assert.True(t, rules[id].Restricted(), id)
				assert.False(t, rules[id].Allows("a.example"), id)
			}
		})
	}
}

func TestRules_Chunks(t *testing.T) {
	r := New()
	f := &fakePlugin{}
	require.NoError(t, r.Add("targets", f.registration()))
	ids := make([]string, MaxNotesPerCall+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d", i)
	}
	rules := r.Rules(context.Background(), ids)
	assert.Len(t, rules, len(ids))
	require.Len(t, f.calls, 2)
	assert.Len(t, f.calls[0], MaxNotesPerCall)
	assert.Len(t, f.calls[1], 3)
}

func TestRules_CopiesHosts(t *testing.T) {
	hosts := []string{"a.example"}
	r := New()
	f := &fakePlugin{targets: map[string]plugin.DeliveryTargets{"n1": {Mode: plugin.DeliveryTargetsInclude, Hosts: hosts}}}
	require.NoError(t, r.Add("targets", f.registration()))
	rule := r.Rule(context.Background(), "n1")
	hosts[0] = "b.example"
	assert.True(t, rule.Allows("a.example"), "the plugin must not be able to change a rule after returning it")
}

// An unknown requester ("") may see an exclude note: it is not one of the
// excluded hosts as far as the server can tell.
func TestRules_UnknownHostForExclude(t *testing.T) {
	r := New()
	f := &fakePlugin{targets: map[string]plugin.DeliveryTargets{"n1": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}}}}
	require.NoError(t, r.Add("targets", f.registration()))
	rule := r.Rule(context.Background(), "n1")
	assert.True(t, rule.Allows(""))
	assert.False(t, rule.Allows("a.example"))
	assert.False(t, DenyAll.Allows(""))
}

func TestDenyAll(t *testing.T) {
	assert.True(t, DenyAll.Restricted())
	assert.False(t, DenyAll.Allows("a.example"))
}

func TestCall_NilContext(t *testing.T) {
	var ctx context.Context
	err := call(ctx, callTimeout, "targets", func(ctx context.Context) error {
		if ctx == nil {
			return errors.New("nil context")
		}
		return nil
	})
	require.NoError(t, err)
}

// slowRegistration ignores its context and returns success after d.
func slowRegistration(d time.Duration) plugin.NoteDeliveryRegistration {
	return plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error {
			time.Sleep(d)
			return nil
		},
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) {
			time.Sleep(d)
			return map[string]plugin.DeliveryTargets{}, nil
		},
	}
}

func TestRules_LateSuccessIsDenied(t *testing.T) {
	r := New()
	r.timeout = 10 * time.Millisecond
	require.NoError(t, r.Add("slow", slowRegistration(50*time.Millisecond)))
	rule := r.Rule(context.Background(), "n1")
	assert.True(t, rule.Restricted())
	assert.False(t, rule.Allows("a.example"))
}

func TestPrepare_LateSuccessIsAnError(t *testing.T) {
	r := New()
	r.timeout = 10 * time.Millisecond
	require.NoError(t, r.Add("slow", slowRegistration(50*time.Millisecond)))
	err := r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{"slow": json.RawMessage(`{}`)})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, errors.Is(err, ErrBadOption))
}

func TestCall_PanicCarriesValue(t *testing.T) {
	err := call(context.Background(), callTimeout, "targets", func(context.Context) error { panic("boom") })
	require.ErrorIs(t, err, errPanicked)
	assert.Contains(t, err.Error(), "boom")
}

func TestRegistry_CallTimeoutDefault(t *testing.T) {
	var nilRegistry *Registry
	assert.Equal(t, callTimeout, nilRegistry.callTimeout())
	assert.Equal(t, callTimeout, New().callTimeout())
}
