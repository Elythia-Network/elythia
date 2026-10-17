package federation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
)

// TestProcess_HooksRunInTheBackgroundGroup checks that the best-effort hooks
// of an inbound Create(Note) start in the group of the server generation
// (#3661): once the group has stopped, neither the processor's hooks nor the
// resolver's chart hook runs. Processor の hook は goHook が箱を選ぶので、呼び出し
// 側で箱を取り違える余地は無い。
func TestProcess_HooksRunInTheBackgroundGroup(t *testing.T) {
	env := newFullProcessor(t, aliceActor)
	fanout := newRecordingFanoutHook()
	env.processor.SetFanoutHook(fanout)
	chart := newRecordingNoteChartHook()
	env.resolver.SetNoteChartHook(chart)
	antenna := newRecordingAntennaHook()
	env.processor.SetAntennaHook(antenna)

	g := bgwork.New()
	env.processor.SetBackground(g)
	env.resolver.SetBackground(g)
	assert.True(t, env.processor.HasBackground())
	assert.True(t, env.resolver.HasBackground())
	require.Zero(t, g.Stop(context.Background()))

	require.NoError(t, env.processor.Process([]byte(noteCreateBody)))
	select {
	case <-fanout.calls:
		t.Fatal("世代を止めた後に fanout のフックが走った")
	case <-antenna.calls:
		t.Fatal("世代を止めた後にアンテナのフックが走った")
	case got := <-chart.calls:
		t.Fatalf("世代を止めた後に chart のフックが走った (note=%s)", got)
	case <-time.After(200 * time.Millisecond):
	}
	// fanout・アンテナ・resolver の chart の 3 つ。
	assert.EqualValues(t, 3, g.Dropped())
}
