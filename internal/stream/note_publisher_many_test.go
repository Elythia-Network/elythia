package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/event"
	"github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FanoutHook は型アサーションで batch 経路を選ぶので、実装が外れると黙って
// 1 件ずつの経路に戻る。コンパイル時に固定する。
var (
	_ timeline.BatchStreamingPublisher = (*NotePublisher)(nil)
	_ BatchPubSubPublisher             = (*event.PubSubService)(nil)
)

// stubBatchPublisher captures PublishMany calls.
type stubBatchPublisher struct {
	stubPublisher
	mu       sync.Mutex
	batches  [][]string
	payloads []any
	errAt    int
	// errs, when set, is returned as is (errAt is ignored).
	errs []error
}

func (s *stubBatchPublisher) PublishMany(_ context.Context, channels []string, payload any) []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]string(nil), channels...))
	s.payloads = append(s.payloads, payload)
	if s.errs != nil {
		return s.errs
	}
	if s.errAt < 0 {
		return nil
	}
	errs := make([]error, len(channels))
	errs[s.errAt] = errors.New("boom")
	return errs
}

func newManyTestNote(t *testing.T) (*model.Note, *model.User, id.Generator) {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	text := "hello"
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Text: &text, Visibility: model.NoteVisibilityPublic}
	return n, &model.User{ID: "u1", Username: "alice"}, idGen
}

func TestNotePublisher_PublishNoteMany_Batch(t *testing.T) {
	n, user, idGen := newManyTestNote(t)
	pub := &stubBatchPublisher{errAt: -1}
	np := NewNotePublisher(pub, idGen)

	topics := []string{"homeTimeline:a", "homeTimeline:b"}
	logs := captureSlog(t)
	np.PublishNoteMany(topics, n, user)
	// 失敗が無ければ何も出さない。
	assert.Empty(t, logs.records(t, "note publisher: publish failed"))
	assert.Empty(t, logs.records(t, "note publisher: publish failed for several topics"))

	require.Len(t, pub.batches, 1)
	assert.Equal(t, topics, pub.batches[0])
	assert.Empty(t, pub.topics, "not one by one")

	// 本文は PublishNote と同じ。
	single := &stubPublisher{}
	NewNotePublisher(single, idGen).PublishNote("homeTimeline:a", n, user)
	require.Len(t, single.payloads, 1)
	want, err := json.Marshal(single.payloads[0])
	require.NoError(t, err)
	got, err := json.Marshal(pub.payloads[0])
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
}

func TestNotePublisher_PublishNoteMany_FallsBackToPublish(t *testing.T) {
	n, user, idGen := newManyTestNote(t)
	pub := &stubPublisher{}
	np := NewNotePublisher(pub, idGen)

	np.PublishNoteMany([]string{"homeTimeline:a", "homeTimeline:b"}, n, user)
	assert.Equal(t, []string{"homeTimeline:a", "homeTimeline:b"}, pub.topics)

	// Publish の失敗は宛先ごとに記録して続ける。
	failing := &stubPublisher{err: errors.New("boom")}
	NewNotePublisher(failing, idGen).PublishNoteMany([]string{"homeTimeline:a"}, n, user)
	assert.Empty(t, failing.topics)

	// 単発の PublishNote の失敗のログにもノート ID を出す。
	logs := captureSlog(t)
	NewNotePublisher(&stubPublisher{err: errors.New("down")}, idGen).PublishNote("homeTimeline:a", n, user)
	recs := logs.records(t, "note publisher: publish failed")
	require.Len(t, recs, 1)
	assert.Equal(t, "homeTimeline:a", recs[0]["topic"])
	assert.Equal(t, n.ID, recs[0]["id"])
	assert.Equal(t, "down", recs[0]["err"])
}

func TestNotePublisher_PublishNoteMany_PartialFailure(t *testing.T) {
	n, user, idGen := newManyTestNote(t)
	pub := &stubBatchPublisher{errAt: 1}
	logs := captureSlog(t)
	NewNotePublisher(pub, idGen).PublishNoteMany([]string{"homeTimeline:a", "homeTimeline:b", "homeTimeline:c"}, n, user)
	require.Len(t, pub.batches, 1)

	// 失敗が 1 件なら、PublishNote と同じ文言・属性 (topic / err) の 1 行。
	recs := logs.records(t, "note publisher: publish failed")
	require.Len(t, recs, 1)
	assert.Equal(t, "homeTimeline:b", recs[0]["topic"])
	assert.Equal(t, n.ID, recs[0]["id"])
	assert.Equal(t, "boom", recs[0]["err"])
	assert.NotContains(t, recs[0], "topics")
	assert.Empty(t, logs.records(t, "note publisher: publish failed for several topics"))
}

// 複数の宛先が失敗したら、別の文言の 1 行に、失敗した宛先を全部と、エラーの
// 種類ごとの件数 (最初に出た順) を出す。
func TestNotePublisher_PublishNoteMany_MixedFailures(t *testing.T) {
	n, user, idGen := newManyTestNote(t)
	e1, e2 := errors.New("i/o timeout"), errors.New("WRONGTYPE")
	pub := &stubBatchPublisher{errs: []error{e1, nil, e2, e1}}
	logs := captureSlog(t)
	NewNotePublisher(pub, idGen).PublishNoteMany([]string{"t0", "t1", "t2", "t3"}, n, user)

	recs := logs.records(t, "note publisher: publish failed for several topics")
	require.Len(t, recs, 1)
	assert.Equal(t, []any{"t0", "t2", "t3"}, recs[0]["topics"])
	assert.EqualValues(t, 3, recs[0]["failed"])
	assert.EqualValues(t, 4, recs[0]["total"])
	assert.Equal(t, n.ID, recs[0]["id"])
	assert.Equal(t, []any{"2: i/o timeout", "1: WRONGTYPE"}, recs[0]["errors"])
	assert.Empty(t, logs.records(t, "note publisher: publish failed"))
}

func TestNotePublisher_PublishNoteMany_FailuresAreSummarized(t *testing.T) {
	n, user, idGen := newManyTestNote(t)
	topics := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		topics = append(topics, fmt.Sprintf("homeTimeline:u%d", i))
	}
	logs := captureSlog(t)
	// まとめて送れない publisher (1 件ずつの経路) でも同じ形で 1 行にまとめる。
	NewNotePublisher(&stubPublisher{err: errors.New("down")}, idGen).PublishNoteMany(topics, n, user)

	recs := logs.records(t, "note publisher: publish failed for several topics")
	require.Len(t, recs, 1)
	want := make([]any, 0, len(topics))
	for _, topic := range topics {
		want = append(want, topic)
	}
	assert.Equal(t, want, recs[0]["topics"])
	assert.EqualValues(t, 8, recs[0]["failed"])
	assert.EqualValues(t, 8, recs[0]["total"])
	assert.Equal(t, []any{"8: down"}, recs[0]["errors"])
}

func TestNotePublisher_PublishNoteMany_Gates(t *testing.T) {
	n, user, idGen := newManyTestNote(t)
	pub := &stubBatchPublisher{errAt: -1}
	np := NewNotePublisher(pub, idGen)

	np.PublishNoteMany(nil, n, user)
	np.PublishNoteMany([]string{"homeTimeline:a"}, nil, user)
	np.PublishNoteMany([]string{"homeTimeline:a"}, n, nil)
	np.PublishNoteMany([]string{"homeTimeline:a"}, n, &model.User{ID: "u1", IsSuspended: true})
	assert.Empty(t, pub.batches, "suspended author and empty input publish nothing")

	NewNotePublisher(nil, idGen).PublishNoteMany([]string{"homeTimeline:a"}, n, user)
}
