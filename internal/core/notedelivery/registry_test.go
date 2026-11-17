package notedelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
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

func TestPrepare_UnknownNameWithNullIsIgnored(t *testing.T) {
	r := New()
	f := &fakePlugin{}
	require.NoError(t, r.Add("targets", f.registration()))
	require.NoError(t, r.Prepare(context.Background(), "n1", "u1", map[string]json.RawMessage{
		"targets": json.RawMessage(`{}`),
		"zzz":     json.RawMessage(`null`),
	}))
	assert.Len(t, f.prepared, 1)
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
		assert.False(t, rule.DeniesAll())
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
	assert.False(t, rules["n1"].DeniesAll(), "an include rule with a host lets that host in")
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
				assert.True(t, rules[id].DeniesAll(), id)
				assert.False(t, rules[id].Allows("a.example"), id)
				assert.True(t, rules[id].Unavailable(), "%s: a failed plugin must be told apart from a deny rule", id)
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
	assert.False(t, DenyAll.Unavailable())
}

func TestRules_HealthyPluginIsAvailable(t *testing.T) {
	r := New()
	require.NoError(t, r.Add("targets", (&fakePlugin{targets: map[string]plugin.DeliveryTargets{
		"n1": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}},
	}}).registration()))
	rules := r.Rules(context.Background(), []string{"n1", "n2"})
	assert.False(t, rules["n1"].Unavailable())
	assert.False(t, rules["n2"].Unavailable())
}

// A value that is not a host name cannot match any stored host, so an exclude
// list carrying one would let the excluded server through. The whole rule is
// treated as unreadable instead (#4202).
func TestRules_NonHostValueDeniesTheNote(t *testing.T) {
	for _, bad := range []string{"https://bad.example", "bad.example/", "*.bad.example", "", "bad example", "bad.example:http", "bad.example:0", "bad.example:65536", "bad..example", "user@bad.example", "[::1", "2001:db8::1"} {
		for _, mode := range []plugin.DeliveryTargetsMode{plugin.DeliveryTargetsExclude, plugin.DeliveryTargetsInclude} {
			t.Run(string(mode)+" "+bad, func(t *testing.T) {
				r := New()
				require.NoError(t, r.Add("targets", (&fakePlugin{targets: map[string]plugin.DeliveryTargets{
					"n1": {Mode: mode, Hosts: []string{"ok.example", bad}},
				}}).registration()))
				rules := r.Rules(context.Background(), []string{"n1", "n2"})
				rule := rules["n1"]
				assert.True(t, rule.Restricted())
				for _, host := range []string{"bad.example", "ok.example", "other.example", ""} {
					assert.False(t, rule.Allows(host), host)
				}
				assert.False(t, rule.Unavailable(), "only the note with the bad value is denied, so it is not an outage")
				assert.False(t, rules["n2"].Restricted(), "other notes keep their rules")
			})
		}
	}
}

// 読めないホストの値を返すプラグインと、エラーを返すプラグインが同じノートに
// 重なったら、どちらが先に呼ばれても障害 (503) として扱う。読めない値の DenyAll
// (404) が残ると、障害のあいだに照会した相手が、ノートを消えたものと扱う (#4202)。
func TestRules_FailureWinsOverNonHostValue(t *testing.T) {
	tests := []struct {
		name               string
		badName, errorName string
	}{
		{name: "bad value first", badName: "a-bad-value", errorName: "b-broken"},
		{name: "error first", badName: "b-bad-value", errorName: "a-broken"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			bad := &fakePlugin{targets: map[string]plugin.DeliveryTargets{
				"n1": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"https://bad.example"}},
			}}
			broken := &fakePlugin{targetsErr: errors.New("db down")}
			require.NoError(t, r.Add(tt.badName, bad.registration()))
			require.NoError(t, r.Add(tt.errorName, broken.registration()))

			rules := r.Rules(context.Background(), []string{"n1", "n2"})
			require.Len(t, bad.calls, 1, "both plugins are asked")
			require.Len(t, broken.calls, 1, "both plugins are asked")
			for _, id := range []string{"n1", "n2"} {
				assert.True(t, rules[id].Unavailable(), id)
				assert.False(t, rules[id].Allows("ok.example"), id)
			}
		})
	}
}

func TestRules_HostValuesThatAreHosts(t *testing.T) {
	for _, good := range []string{"a.example", "A.Example.", "a.example:8443", "パイ.example", "192.0.2.1", "[2001:db8::1]", "[2001:db8::1]:8443", "under_score.example", "localhost"} {
		t.Run(good, func(t *testing.T) {
			r := New()
			require.NoError(t, r.Add("targets", (&fakePlugin{targets: map[string]plugin.DeliveryTargets{
				"n1": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{good}},
			}}).registration()))
			rule := r.Rule(context.Background(), "n1")
			assert.True(t, rule.Allows("other.example"), "a valid host must not deny the whole note")
			assert.False(t, rule.Allows(good))
		})
	}
}

func TestNames(t *testing.T) {
	var nilRegistry *Registry
	assert.Empty(t, nilRegistry.Names())
	r := New()
	require.NoError(t, r.Add("zeta", (&fakePlugin{}).registration()))
	require.NoError(t, r.Add("alpha", (&fakePlugin{}).registration()))
	assert.Equal(t, []string{"alpha", "zeta"}, r.Names())
}

type forgetRecorder struct {
	notes [][]string
	users []string
	err   error
	panic bool
}

func (f *forgetRecorder) registration() plugin.NoteDeliveryRegistration {
	reg := (&fakePlugin{}).registration()
	reg.ForgetNotes = func(ctx context.Context, ids []string) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("no deadline")
		}
		if f.panic {
			panic("boom")
		}
		f.notes = append(f.notes, ids)
		return f.err
	}
	reg.ForgetUser = func(ctx context.Context, userID string) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("no deadline")
		}
		if f.panic {
			panic("boom")
		}
		f.users = append(f.users, userID)
		return f.err
	}
	return reg
}

func TestForgetNotes(t *testing.T) {
	r := New()
	broken := &forgetRecorder{panic: true}
	failing := &forgetRecorder{err: errors.New("db down")}
	ok := &forgetRecorder{}
	require.NoError(t, r.Add("a-broken", broken.registration()))
	require.NoError(t, r.Add("b-failing", failing.registration()))
	require.NoError(t, r.Add("c-ok", ok.registration()))
	require.NoError(t, r.Add("d-without", (&fakePlugin{}).registration()))

	ids := make([]string, MaxNotesPerCall+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("n%d", i)
	}
	r.ForgetNotes(context.Background(), ids)
	require.Len(t, ok.notes, 2, "IDs are handed over in chunks")
	assert.Len(t, ok.notes[0], MaxNotesPerCall)
	assert.Equal(t, []string{ids[MaxNotesPerCall]}, ok.notes[1])
	assert.Len(t, failing.notes, 2, "a failing plugin does not stop the others")

	r.ForgetNotes(context.Background(), nil)
	assert.Len(t, ok.notes, 2, "nothing to forget calls nothing")
	var nilRegistry *Registry
	nilRegistry.ForgetNotes(context.Background(), ids)
}

func TestForgetUser(t *testing.T) {
	r := New()
	failing := &forgetRecorder{err: errors.New("db down")}
	ok := &forgetRecorder{}
	require.NoError(t, r.Add("a-failing", failing.registration()))
	require.NoError(t, r.Add("b-broken", (&forgetRecorder{panic: true}).registration()))
	require.NoError(t, r.Add("c-ok", ok.registration()))
	require.NoError(t, r.Add("d-without", (&fakePlugin{}).registration()))
	r.ForgetUser(context.Background(), "u1")
	assert.Equal(t, []string{"u1"}, ok.users)
	assert.Equal(t, []string{"u1"}, failing.users)
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

// The hosts a plugin returns and the host compared with them are put in the
// stored form first, so spelling differences cannot let an excluded server
// through (#4199).
func TestRule_ComparesCanonicalHosts(t *testing.T) {
	spellings := []string{"Bad.Example", "bad.example.", "bad.example:443", "bad.example:80", " BAD.example.:443 ", "bad.example"}
	for _, listed := range spellings {
		for _, asked := range spellings {
			t.Run(listed+" vs "+asked, func(t *testing.T) {
				for _, mode := range []plugin.DeliveryTargetsMode{plugin.DeliveryTargetsExclude, plugin.DeliveryTargetsInclude} {
					r := New()
					require.NoError(t, r.Add("targets", (&fakePlugin{targets: map[string]plugin.DeliveryTargets{
						"n1": {Mode: mode, Hosts: []string{listed}},
					}}).registration()))
					rule := r.Rule(context.Background(), "n1")
					assert.Equal(t, mode == plugin.DeliveryTargetsInclude, rule.Allows(asked), "mode %s", mode)
				}
			})
		}
	}
}

func TestRule_ComparesCanonicalHosts_IDNAndPorts(t *testing.T) {
	r := New()
	require.NoError(t, r.Add("targets", (&fakePlugin{targets: map[string]plugin.DeliveryTargets{
		"idn":  {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"パイ.example"}},
		"port": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"bad.example:8443"}},
	}}).registration()))
	idn := r.Rule(context.Background(), "idn")
	assert.False(t, idn.Allows("xn--eckve.example"))
	assert.False(t, idn.Allows("パイ.example"))
	assert.True(t, idn.Allows("other.example"))

	// 既定でないポートは別のサーバーのまま扱う (保存される host もポートを残す)。
	port := r.Rule(context.Background(), "port")
	assert.False(t, port.Allows("bad.example:8443"))
	assert.True(t, port.Allows("bad.example"))
}

func TestCanonicalHost(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		"  ":                 "",
		".":                  "",
		"Bad.Example.":       "bad.example",
		"bad.example:443":    "bad.example",
		"bad.example:80":     "bad.example",
		"bad.example:8443":   "bad.example:8443",
		"パイ.example":         "xn--eckve.example",
		"[2001:db8::1]":      "[2001:db8::1]",
		"[2001:db8::1]:443":  "[2001:db8::1]",
		"[2001:db8::1]:8443": "[2001:db8::1]:8443",
	} {
		assert.Equal(t, want, canonicalHost(in), "canonicalHost(%q)", in)
	}
}

// blockingForgetter registers a plugin whose ForgetNotes blocks until
// release is closed (ignoring ctx, as a stuck plugin would).
func blockingForgetter(t *testing.T, r *Registry) (release chan struct{}, calls *atomic.Int32) {
	t.Helper()
	release = make(chan struct{})
	calls = &atomic.Int32{}
	reg := (&fakePlugin{}).registration()
	reg.ForgetNotes = func(context.Context, []string) error {
		calls.Add(1)
		<-release
		return nil
	}
	require.NoError(t, r.Add("targets", reg))
	return release, calls
}

// The caller does not wait, and the call runs in the group of the server
// generation so stopping it waits for the call (#4202).
func TestForgetNotesInBackground_RunsInGroup(t *testing.T) {
	r := New()
	r.timeout = time.Minute
	release, calls := blockingForgetter(t, r)
	g := bgwork.New()

	done := make(chan struct{})
	go func() {
		r.ForgetNotesInBackground(g, []string{"n1"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ForgetNotesInBackground waited for the plugin")
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(1), g.Running(), "the call is tracked by the generation group")
	close(release)
	require.Eventually(t, func() bool { return g.Running() == 0 && r.forgetsRunning.Load() == 0 }, 2*time.Second, 5*time.Millisecond)
}

// A stuck plugin does not pile up goroutines: beyond MaxBackgroundForgets
// running calls, more are dropped (#4202).
func TestForgetNotesInBackground_BoundsRunningCalls(t *testing.T) {
	r := New()
	r.timeout = time.Minute
	release, calls := blockingForgetter(t, r)
	g := bgwork.New()
	for i := 0; i < MaxBackgroundForgets+3; i++ {
		r.ForgetNotesInBackground(g, []string{fmt.Sprintf("n%d", i)})
	}
	require.Eventually(t, func() bool { return calls.Load() == MaxBackgroundForgets }, 2*time.Second, 5*time.Millisecond)
	assert.Never(t, func() bool { return calls.Load() > MaxBackgroundForgets }, 100*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, int64(MaxBackgroundForgets), g.Running())

	close(release)
	require.Eventually(t, func() bool { return r.forgetsRunning.Load() == 0 }, 2*time.Second, 5*time.Millisecond)
	r.ForgetNotesInBackground(g, []string{"after"})
	require.Eventually(t, func() bool { return calls.Load() == MaxBackgroundForgets+1 }, 2*time.Second, 5*time.Millisecond, "slots are given back")
}

// After the generation stopped, the call is not started and its slot is
// given back.
func TestForgetNotesInBackground_StoppedGroup(t *testing.T) {
	r := New()
	release, calls := blockingForgetter(t, r)
	close(release)
	g := bgwork.New()
	g.Stop(context.Background())
	r.ForgetNotesInBackground(g, []string{"n1"})
	assert.Equal(t, int32(0), r.forgetsRunning.Load())
	assert.Equal(t, int64(1), g.Dropped())
	assert.Never(t, func() bool { return calls.Load() > 0 }, 50*time.Millisecond, 5*time.Millisecond)
}

func TestForgetNotesInBackground_NothingToDo(t *testing.T) {
	var nilRegistry *Registry
	nilRegistry.ForgetNotesInBackground(nil, []string{"n1"})
	r := New()
	r.ForgetNotesInBackground(nil, []string{"n1"})
	_, calls := blockingForgetter(t, r)
	r.ForgetNotesInBackground(nil, nil)
	assert.Equal(t, int32(0), r.forgetsRunning.Load())
	assert.Equal(t, int32(0), calls.Load())
}

func TestNewForServer_LocalHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://Local.Example/":       "local.example",
		"https://local.example:443":    "local.example",
		"https://local.example:8443/":  "local.example:8443",
		"https://パイ.example":           "xn--eckve.example",
		"http://local.example:80/path": "local.example",
		"":                             "",
		"::not a url":                  "",
	} {
		assert.Equal(t, want, NewForServer(in).LocalHost(), in)
	}
	var nilRegistry *Registry
	assert.Equal(t, "", nilRegistry.LocalHost())
	assert.Equal(t, "", New().LocalHost())
}

// A viewer who is not signed in is judged as a fetch from this server's own
// host, the same way the conditions narrow remote servers (#4203).
func TestHiddenFromVisitors(t *testing.T) {
	r := NewForServer("https://local.example")
	require.NoError(t, r.Add("targets", (&fakePlugin{targets: map[string]plugin.DeliveryTargets{
		"include-local":   {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example", "Local.Example"}},
		"include-remote":  {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}},
		"exclude-local":   {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example:443"}},
		"exclude-remote":  {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}},
		"not-a-host":      {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"https://a.example"}},
		"include-nothing": {Mode: plugin.DeliveryTargetsInclude},
	}}).registration()))
	ids := []string{"include-local", "include-remote", "exclude-local", "exclude-remote", "not-a-host", "include-nothing", "unrestricted"}
	assert.Equal(t, map[string]bool{
		"include-remote":  true,
		"exclude-local":   true,
		"not-a-host":      true,
		"include-nothing": true,
	}, r.HiddenFromVisitors(context.Background(), ids))
}

func TestHiddenFromVisitors_PluginFailureHides(t *testing.T) {
	r := NewForServer("https://local.example")
	require.NoError(t, r.Add("targets", (&fakePlugin{targetsErr: errors.New("db down")}).registration()))
	assert.Equal(t, map[string]bool{"n1": true, "n2": true}, r.HiddenFromVisitors(context.Background(), []string{"n1", "n2"}))
}

func TestHiddenFromVisitors_NothingToJudge(t *testing.T) {
	assert.Nil(t, NewForServer("https://local.example").HiddenFromVisitors(context.Background(), []string{"n1"}), "no plugin")
	var nilRegistry *Registry
	assert.Nil(t, nilRegistry.HiddenFromVisitors(context.Background(), []string{"n1"}))
	r := NewForServer("https://local.example")
	f := &fakePlugin{}
	require.NoError(t, r.Add("targets", f.registration()))
	assert.Nil(t, r.HiddenFromVisitors(context.Background(), nil))
	assert.Empty(t, f.calls, "no note, no call")
}
