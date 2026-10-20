package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
)

// stepPanicLogMsg is the message runShutdownSteps logs for a step that
// panicked, as it appears in the JSON log.
const stepPanicLogMsg = `"msg":"server: a shutdown step panicked; running the rest"`

// stepNames returns the names of steps in order.
func stepNames(steps []shutdownStep) []string {
	names := make([]string, 0, len(steps))
	for _, st := range steps {
		names = append(names, st.name)
	}
	return names
}

// TestServer_ShutdownStepsAreInTheirOrder pins the names and the order of
// the steps of stopping a generation and of rolling back a failed
// construction (#3711). The reasons for the order are in docs/architecture.md
// 4.3; change this table and the doc together.
func TestServer_ShutdownStepsAreInTheirOrder(t *testing.T) {
	s := &Server{}
	assert.Equal(t, []string{
		"streaming", "queue", "background", "lifecycle", "chart",
		"lateQueue", "queueDriver", "pubsub", "echo",
	}, stepNames(s.shutdownSteps()))
	assert.Equal(t, []string{
		"pluginGoGate", "lifecycle", "background", "releaseParts",
		"queueDriver", "streaming", "pubsub",
	}, stepNames(s.cleanupSteps()))
}

// TestRunShutdownSteps_RecoversAPanickingStep checks that when a step panics,
// runShutdownSteps logs it with the step's name, the panic value and the
// stack, still runs every later step in order with the same ctx, and returns
// the errors of the panicked steps and the error a step returned, in step
// order (#3711).
func TestRunShutdownSteps_RecoversAPanickingStep(t *testing.T) {
	logs := captureSlog(t)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "shutdown")
	errThird := errors.New("third failed")
	var got []string
	var steps []shutdownStep
	for _, name := range []string{"first", "second", "third", "fourth", "fifth"} {
		steps = append(steps, shutdownStep{name: name, run: func(c context.Context) error {
			assert.Equal(t, "shutdown", c.Value(key{}), "step %s did not get the ctx", name)
			got = append(got, name)
			switch name {
			case "second", "fourth":
				panic(name + " failed")
			case "third":
				return errThird
			}
			return nil
		}})
	}
	err := runShutdownSteps(ctx, steps)
	assert.Equal(t, []string{"first", "second", "third", "fourth", "fifth"}, got)
	require.ErrorIs(t, err, errShutdownStep)
	require.ErrorIs(t, err, errThird)
	errs := panickedParts(t, err)
	require.Len(t, errs, 3)
	assert.ErrorIs(t, errs[0], errShutdownStep)
	assert.ErrorContains(t, errs[0], `step "second" panicked: second failed`)
	assert.Equal(t, errThird, errs[1])
	assert.ErrorIs(t, errs[2], errShutdownStep)
	assert.ErrorContains(t, errs[2], `step "fourth" panicked: fourth failed`)

	out := logs.String()
	for _, name := range []string{"second", "fourth"} {
		assert.Contains(t, out, `"step":"`+name+`"`)
		assert.Contains(t, out, `"panic":"`+name+` failed"`)
	}
	// スタックはrecoverした段の位置を含む。
	assert.Contains(t, out, `"stack":"goroutine `)
	assert.Contains(t, out, "TestRunShutdownSteps_RecoversAPanickingStep")
	assert.Equal(t, 2, strings.Count(out, stepPanicLogMsg))
}

// TestRunShutdownSteps_ReturnsASingleErrorAsIs checks that runShutdownSteps
// returns nil when no step failed and the error itself when exactly one step
// failed, so that a stop without a panic returns echo's error unwrapped, as
// before #3711.
func TestRunShutdownSteps_ReturnsASingleErrorAsIs(t *testing.T) {
	captureSlog(t)
	ok := shutdownStep{name: "ok", run: func(context.Context) error { return nil }}
	assert.NoError(t, runShutdownSteps(context.Background(), []shutdownStep{ok, ok}))

	errOne := errors.New("one")
	err := runShutdownSteps(context.Background(), []shutdownStep{ok,
		{name: "one", run: func(context.Context) error { return errOne }}, ok})
	assert.Same(t, errOne, err, "the single error was wrapped")

	err = runShutdownSteps(context.Background(), []shutdownStep{ok,
		{name: "boom", run: func(context.Context) error { panic("boom") }}, ok})
	require.ErrorIs(t, err, errShutdownStep)
	_, joined := err.(interface{ Unwrap() []error })
	assert.False(t, joined, "a single panic was joined")
}

// panickingCloseDriver is a closeRecordingDriver whose Close panics after
// recording itself.
type panickingCloseDriver struct {
	closeRecordingDriver
}

func (d *panickingCloseDriver) Close() error {
	_ = d.closeRecordingDriver.Close()
	panic("queue driver close failed")
}

// TestServer_StopBackgroundGoesOnAfterAPanickingStep makes the queueDriver
// step panic, along with a part of the lifecycle list, and checks that the
// later steps still run (the Redis subscriptions close and echo's Shutdown
// runs), and that StopBackground returns the panic of the step naming it,
// the panic of the part and echo's error (#3711).
func TestServer_StopBackgroundGoesOnAfterAPanickingStep(t *testing.T) {
	logs := captureSlog(t)
	mr := miniredis.RunT(t)
	s, log := panicStopServer(t, mr, []string{"first", "second", "third"}, map[string]bool{"second": true})
	s.queueDriver = &panickingCloseDriver{closeRecordingDriver{log: log}}
	serveBlockedRequest(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := s.StopBackground(ctx)
	assert.Equal(t, []string{"first", "second", "third", "queueDriver.Close"}, *log)
	require.ErrorIs(t, err, errShutdownStep)
	assert.ErrorContains(t, err, `step "queueDriver" panicked: queue driver close failed`)
	require.ErrorIs(t, err, errLifecycleStop)
	assert.ErrorContains(t, err, `part "second" panicked`)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the echo step did not run after the panic")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Zero(c, mr.PubSubNumPat(), "the Redis subscriptions were not closed after the panic")
	}, 5*time.Second, 20*time.Millisecond)
	out := logs.String()
	assert.Contains(t, out, `"step":"queueDriver"`)
	assert.Contains(t, out, `"panic":"queue driver close failed"`)
	assert.Contains(t, out, "panickingCloseDriver")
	// 2回目は1回目の結果を返し、止め直さない。
	assert.Equal(t, err, s.StopBackground(context.Background()))
	assert.Len(t, *log, 4)
}

// TestServer_ShutdownAfterAPanickingQueueStep checks what the later steps do
// when the queue step panics (#3711): they do not wait for a queue that has
// no channel to wait on, and the goroutines of the generation are collected
// after the lifecycle list (as for a queue that did not stop in time), not
// before it. The case without a panic is the control: the goroutines are
// collected before the list there.
func TestServer_ShutdownAfterAPanickingQueueStep(t *testing.T) {
	captureSlog(t)
	for _, tc := range []struct {
		name       string
		panicQueue bool
	}{
		{name: "the queue step panics", panicQueue: true},
		{name: "control: the queue stops", panicQueue: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			s, log := panicStopServer(t, mr, nil, nil)
			s.background = bgwork.New()
			collectedBeforeList := make(chan bool, 1)
			s.addPart("part", nil, func(context.Context) {
				select {
				case <-s.background.Stopping():
					collectedBeforeList <- true
				default:
					collectedBeforeList <- false
				}
			})
			steps := s.shutdownSteps()
			if tc.panicQueue {
				for i := range steps {
					if steps[i].name == "queue" {
						steps[i].run = func(context.Context) error { panic("queue stop failed") }
					}
				}
			}
			done := make(chan error, 1)
			go func() { done <- runShutdownSteps(context.Background(), steps) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the steps after the queue step did not return")
			}
			if tc.panicQueue {
				require.ErrorIs(t, err, errShutdownStep)
				assert.ErrorContains(t, err, `step "queue" panicked: queue stop failed`)
			} else {
				require.NoError(t, err)
			}
			// 段は同期に走るので、返った後に記録が無ければlifecycleの段は走っていない。
			// 待つと、段が飛ばされたときにgo testの上限まで固まる。
			select {
			case before := <-collectedBeforeList:
				assert.Equal(t, !tc.panicQueue, before, "where the goroutines of the generation were collected")
			default:
				t.Fatal("the lifecycle step did not run after the queue step")
			}
			select {
			case <-s.background.Stopping():
			default:
				t.Error("the goroutines of the generation were not collected")
			}
			assert.Equal(t, []string{"queueDriver.Close"}, *log)
			assert.EventuallyWithT(t, func(c *assert.CollectT) {
				assert.Zero(c, mr.PubSubNumPat())
			}, 5*time.Second, 20*time.Millisecond)
		})
	}
}

// TestServer_FailedConstructionCleanupGoesOnAfterAPanickingStep makes the
// queueDriver step of the rollback of a failed construction panic and checks
// that the later steps still run (the Redis subscriptions close), that the
// panic does not leave cleanupConstruction and that it is logged once with
// the step's name (#3711).
func TestServer_FailedConstructionCleanupGoesOnAfterAPanickingStep(t *testing.T) {
	logs := captureSlog(t)
	mr := miniredis.RunT(t)
	s, log := panicStopServer(t, mr, []string{"first"}, nil)
	s.queueDriver = &panickingCloseDriver{closeRecordingDriver{log: log}}
	require.NotPanics(t, s.cleanupConstruction)
	assert.Equal(t, []string{"first", "queueDriver.Close"}, *log)
	assert.Nil(t, s.lifecycle.parts)
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Zero(c, mr.PubSubNumPat(), "the Redis subscriptions were not closed after the panic")
	}, 5*time.Second, 20*time.Millisecond)
	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, `"panic":"queue driver close failed"`), "the panic was not logged once")
	assert.Contains(t, out, `"step":"queueDriver"`)
}
