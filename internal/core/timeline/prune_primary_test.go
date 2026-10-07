package timeline

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// TestPruneDangling_KeepsRowsPresentOnPrimary は #2757 を固定する。
//
// timeline の読み取り (`FindManyByIDsWithUser`) はレプリカに振られるので、
// 複製前の行は引けない。fanout は primary への commit 直後に走るため、
// 「生きているのに引けない」窓がある。そこで prune すると **その note は
// Redis list から消え、戻す経路が無い**。
//
// DB fallback があるから安全、とは言えない。fallback は Hybrid 以外では別
// メソッドで、いずれも `AllowPartial` を渡されると 1 件でも解決できたページでは
// 走らず (#3448)、しかも一度消えた ID は list に戻らない。
func TestPruneDangling_KeepsRowsPresentOnPrimary(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	testRedis.FlushAll(context.Background())

	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	noteRepo := testutil.NewMockNoteRepository()
	svc := NewService(fanout, noteRepo, testutil.NewMockFollowingRepository())

	name := GlobalTimeline
	replicaLag := idGen.Generate(time.Now())
	reallyGone := idGen.Generate(time.Now().Add(time.Millisecond))
	for _, id := range []string{replicaLag, reallyGone} {
		require.NoError(t, fanout.Push(context.Background(), name, id, 100))
	}
	// primary には replicaLag だけが在る (= 複製が追いついていない状態)。
	noteRepo.Notes[replicaLag] = &model.Note{ID: replicaLag}

	svc.pruneDangling(context.Background(), []Name{name}, []string{replicaLag, reallyGone})

	got, err := fanout.Get(context.Background(), name, "", "", 10)
	require.NoError(t, err)
	assert.Contains(t, got, replicaLag, "primary に在る行は消さない")
	assert.NotContains(t, got, reallyGone, "primary にも無い行は消す")
}

// TestPruneDangling_FailSafeOnPrimaryError は、確かめられないときに何もしない
// ことを固定する。
func TestPruneDangling_FailSafeOnPrimaryError(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	testRedis.FlushAll(context.Background())

	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.ExistingOnPrimaryErr = assert.AnError
	svc := NewService(fanout, noteRepo, testutil.NewMockFollowingRepository())

	name := GlobalTimeline
	id := idGen.Generate(time.Now())
	require.NoError(t, fanout.Push(context.Background(), name, id, 100))

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc.pruneDangling(context.Background(), []Name{name}, []string{id})

	got, err := fanout.Get(context.Background(), name, "", "", 10)
	require.NoError(t, err)
	assert.Contains(t, got, id, "確かめられないときは消さない")
	assert.Contains(t, buf.String(), "primary existence check failed", "理由が warn に出る")
}

// replicaLagRepo は「primary には在るがレプリカにはまだ無い」状態を模す。
//
// testutil.MockNoteRepository は単一ストアなので FindManyByIDsWithUser (=
// レプリカ相当) と ExistingNoteIDsOnPrimary (= primary) が同じ map を返し、
// 複製遅延を表現できない。埋め込んだうえで前者だけを細らせる。
type replicaLagRepo struct {
	*testutil.MockNoteRepository
	lagging map[string]struct{}
}

func (r *replicaLagRepo) FindManyByIDsWithUser(ids []string) ([]*model.Note, error) {
	notes, err := r.MockNoteRepository.FindManyByIDsWithUser(ids)
	if err != nil {
		return nil, err
	}
	out := notes[:0]
	for _, n := range notes {
		if _, lag := r.lagging[n.ID]; !lag {
			out = append(out, n)
		}
	}
	return out, nil
}

// TestTimelines_PrimaryConfirmationOnEveryPath は、確認が **4 つの公開読み取り
// 経路すべて**で効くことを固定する。
//
// prune 自体は pruneDangling に閉じているが、確認をそこから呼び出し元へ
// 移すようなリファクタ (例: home だけに入れる) は、pruneDangling を直接叩く
// テストでは素通りする。
func TestTimelines_PrimaryConfirmationOnEveryPath(t *testing.T) {
	testutil.SkipIfNoDocker(t)

	cases := []struct {
		name string
		key  Name
		read func(*Service, *model.User) error
	}{
		{"home", HomeTimelineName("u1"), func(s *Service, u *model.User) error {
			_, err := s.HomeTimeline(context.Background(), u, "", "", 10, TimelineFilter{})
			return err
		}},
		{"local", LocalTimeline, func(s *Service, u *model.User) error {
			_, err := s.LocalTimeline(context.Background(), u, "", "", 10, TimelineFilter{})
			return err
		}},
		{"global", GlobalTimeline, func(s *Service, u *model.User) error {
			_, err := s.GlobalTimeline(context.Background(), u, "", "", 10, TimelineFilter{})
			return err
		}},
		{"hybrid", HomeTimelineName("u1"), func(s *Service, u *model.User) error {
			_, err := s.HybridTimeline(context.Background(), u, "", "", 10, TimelineFilter{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testRedis.FlushAll(context.Background())
			fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
			fanout.randFn = func() float64 { return 1.0 }
			base := testutil.NewMockNoteRepository()
			lagID := idGen.Generate(time.Now())
			base.Notes[lagID] = &model.Note{ID: lagID, UserID: "u1", Visibility: model.NoteVisibilityPublic}
			repo := &replicaLagRepo{MockNoteRepository: base, lagging: map[string]struct{}{lagID: {}}}
			svc := NewService(fanout, repo, testutil.NewMockFollowingRepository())

			// **消える側も置く。** lagID だけだと、fixture が壊れて dangling が
			// 空になったとき pruneDangling の早期 return で素通りし、確認を
			// 外しても通ってしまう。
			goneID := idGen.Generate(time.Now().Add(time.Millisecond))
			require.NoError(t, fanout.Push(context.Background(), tc.key, lagID, 100))
			require.NoError(t, fanout.Push(context.Background(), tc.key, goneID, 100))
			require.NoError(t, tc.read(svc, &model.User{ID: "u1"}))

			got, err := fanout.Get(context.Background(), tc.key, "", "", 10)
			require.NoError(t, err)
			assert.Contains(t, got, lagID,
				"複製前の行 (primary には在る) を list から消してはいけない")
			assert.NotContains(t, got, goneID,
				"primary にも無い行は消える (prune 自体が動いていることの確認)")
		})
	}
}
