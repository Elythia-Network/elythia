package timeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFanoutToggle is a FanoutToggleProvider stub.
type fakeFanoutToggle struct {
	enabled bool
}

func (f *fakeFanoutToggle) FanoutTimelineEnabled() bool { return f.enabled }

// --- FanoutToggleProvider (production adapter) ---

func TestNewMetaFanoutToggle_ReadsFromMeta(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		repo := &stubMetaRepo{meta: &model.Meta{EnableFanoutTimeline: enabled}}
		assert.Equal(t, enabled, NewMetaFanoutToggle(repo).FanoutTimelineEnabled())
	}
}

func TestNewMetaFanoutToggle_FailsOpen(t *testing.T) {
	// meta が読めないときは有効側に倒す (既定値が true のため)。
	assert.True(t, NewMetaFanoutToggle(&stubMetaRepo{err: errors.New("db down")}).FanoutTimelineEnabled())
	assert.True(t, NewMetaFanoutToggle(&stubMetaRepo{meta: nil}).FanoutTimelineEnabled())
	assert.True(t, NewMetaFanoutToggle(nil).FanoutTimelineEnabled())

	var p *metaRepoCacheLimits
	assert.True(t, p.FanoutTimelineEnabled())
}

// --- FanoutHook push gate ---

func TestFanoutHook_ToggleDisabledSkipsPush(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	hook := NewFanoutHook(fanout, testutil.NewMockFollowingRepository())
	hook.SetFanoutToggle(&fakeFanoutToggle{enabled: false})

	author := &model.User{ID: "author"}
	noteID := idGen.Generate(time.Now())
	hook.OnNoteCreated(&model.Note{ID: noteID, UserID: author.ID, Visibility: model.NoteVisibilityPublic}, author)

	for _, name := range []Name{HomeTimelineName(author.ID), UserTimelineName(author.ID), LocalTimeline, GlobalTimeline} {
		ids, err := fanout.Get(ctx, name, "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, ids, string(name)+" must stay empty while FTT is off")
	}
}

func TestFanoutHook_ToggleEnabledPushes(t *testing.T) {
	ctx := context.Background()
	testRedis.FlushAll(ctx)
	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	hook := NewFanoutHook(fanout, testutil.NewMockFollowingRepository())
	hook.SetFanoutToggle(&fakeFanoutToggle{enabled: true})

	author := &model.User{ID: "author"}
	noteID := idGen.Generate(time.Now())
	hook.OnNoteCreated(&model.Note{ID: noteID, UserID: author.ID, Visibility: model.NoteVisibilityPublic}, author)

	ids, err := fanout.Get(ctx, GlobalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, ids)
}

func TestFanoutHook_FanoutEnabledDefaultsToTrue(t *testing.T) {
	assert.True(t, (&FanoutHook{}).fanoutEnabled())
}

func TestFanoutEnabledFrom_UnsetContext(t *testing.T) {
	assert.True(t, fanoutEnabledFrom(context.Background()))
	assert.False(t, fanoutEnabledFrom(withFanoutEnabled(context.Background(), false)))
}

// --- Service read gate ---

// FTT を切ったら Redis には触らないことを、閉じた client (触れば必ずエラー)
// で確認する。
func TestService_ToggleDisabledSkipsRedis(t *testing.T) {
	ctx := context.Background()
	viewer := &model.User{ID: "viewer"}
	reads := map[string]func(svc *Service) error{
		"home": func(svc *Service) error {
			_, err := svc.HomeTimeline(ctx, viewer, "", "", 10, TimelineFilter{})
			return err
		},
		"local": func(svc *Service) error {
			_, err := svc.LocalTimeline(ctx, viewer, "", "", 10, TimelineFilter{})
			return err
		},
		"global": func(svc *Service) error {
			_, err := svc.GlobalTimeline(ctx, viewer, "", "", 10, TimelineFilter{})
			return err
		},
		"hybrid": func(svc *Service) error {
			_, err := svc.HybridTimeline(ctx, viewer, "", "", 10, TimelineFilter{})
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			svc := NewService(NewFanoutTimelineService(closedClient(t), idGen, ""), testutil.NewMockNoteRepository(), testutil.NewMockFollowingRepository())

			svc.SetFanoutToggle(&fakeFanoutToggle{enabled: true})
			assert.Error(t, read(svc), "FTT 有効なら Redis を読んでエラーになる")

			svc.SetFanoutToggle(&fakeFanoutToggle{enabled: false})
			assert.NoError(t, read(svc), "FTT 無効なら Redis を触らず DB へ直行する")
		})
	}
}

func TestService_FanoutEnabledDefaultsToTrue(t *testing.T) {
	assert.True(t, (&Service{}).fanoutEnabled())
}

// --- fallbackRange ---

func TestFallbackRange(t *testing.T) {
	tests := []struct {
		name              string
		ids               []string
		sinceID, untilID  string
		wantSince, wantTo string
	}{
		{name: "no ids keeps the original range", sinceID: "s", untilID: "u", wantSince: "s", wantTo: "u"},
		{name: "descending narrows untilId to the oldest read id", ids: []string{"c", "b", "a"}, untilID: "u", wantTo: "a"},
		{name: "descending keeps sinceId", ids: []string{"c", "a"}, sinceID: "s", untilID: "u", wantSince: "s", wantTo: "a"},
		{name: "ascending narrows sinceId to the newest read id", ids: []string{"a", "b", "c"}, sinceID: "s", wantSince: "c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSince, gotUntil := fallbackRange(notesWithIDs(tt.ids), tt.sinceID, tt.untilID)
			assert.Equal(t, tt.wantSince, gotSince)
			assert.Equal(t, tt.wantTo, gotUntil)
		})
	}
}

// notesWithIDs builds the minimal notes fallbackRange looks at (id only)。
func notesWithIDs(ids []string) []*model.Note {
	if len(ids) == 0 {
		return nil
	}
	out := make([]*model.Note, 0, len(ids))
	for _, id := range ids {
		out = append(out, &model.Note{ID: id})
	}
	return out
}
