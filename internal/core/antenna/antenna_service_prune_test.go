package antenna

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/model"
)

func newPruneTestService(t *testing.T) (*Service, *redis.Client) {
	t.Helper()
	s, _ := newSvc(t)
	return s, testRedis.Client
}

func redisZ(member string) redis.Z { return redis.Z{Score: 0, Member: member} }

// TestMissingIDs は解決できなかった ID の抽出を固定する。
func TestMissingIDs(t *testing.T) {
	notes := []*model.Note{{ID: "b"}}
	assert.Equal(t, []string{"a", "c"}, missingIDs([]string{"a", "b", "c"}, notes),
		"入力順を保って未解決分だけ返す")
	assert.Equal(t, []string{"a", "b"}, missingIDs([]string{"a", "b"}, nil),
		"1 件も解決できなければ全件")
	assert.Nil(t, missingIDs([]string{"b"}, notes), "全件解決できたら空")
}

// TestPruneDangling_UsesDetachedContext は、リクエストの ctx がキャンセル
// 済みでも自己修復が走ることを固定する。
//
// **症状が出るのはリロード時 = 前のリクエストを中断する操作**なので、
// リクエストの ctx をそのまま持ち込むと直したい場面ほど空振りする
// (#2718 review MEDIUM-4 と同じ)。handler 経由だと prune の手前で読み取りが
// 先に失敗するため、ここは service を直接叩いて分離する。
func TestPruneDangling_UsesDetachedContext(t *testing.T) {
	s, client := newPruneTestService(t)
	s.SetPrimaryNoteExistence(&fakePrimary{existing: map[string]bool{"alive": true}})
	key := streamKey("a1")
	alive, gone := "alive", "gone"
	require.NoError(t, client.ZAdd(context.Background(), key, redisZ(alive), redisZ(gone)).Err())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.PruneDangling(ctx, "a1", []string{alive, gone}, []*model.Note{{ID: alive}})

	ids, err := client.ZRange(context.Background(), key, 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{alive}, ids, "ctx がキャンセル済みでも prune は走る")
}

// TestPruneDangling_NoOpWhenNothingDangling は、消すものが無いときに Redis を
// 触らないことを固定する。
//
// **primary には何も無い fakePrimary を渡す。** 「primary に在るから消さない」
// で通ってしまうと、missingIDs をバイパスする変異を捕まえられない。
func TestPruneDangling_NoOpWhenNothingDangling(t *testing.T) {
	s, client := newPruneTestService(t)
	s.SetPrimaryNoteExistence(&fakePrimary{})
	key := streamKey("a1")
	alive := "alive"
	require.NoError(t, client.ZAdd(context.Background(), key, redisZ(alive)).Err())

	s.PruneDangling(context.Background(), "a1", []string{alive}, []*model.Note{{ID: alive}})

	ids, err := client.ZRange(context.Background(), key, 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{alive}, ids)
}

// fakePrimary は primary 側の存在確認を差し替える。
type fakePrimary struct {
	existing map[string]bool
	err      error
}

func (f *fakePrimary) ExistingNoteIDsOnPrimary(ids []string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for _, id := range ids {
		if f.existing[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// TestPruneDangling_KeepsRowsPresentOnPrimary は #2719 のレビューで見つかった
// 穴を固定する。
//
// 読み取りはレプリカに振られるので、複製前の行は引けない。ID の時刻で猶予を
// 判定する方式は**リモート note に効かない** — リモートの ID は AP の
// `published` から発番されるため「たった今 INSERT されたが ID は数時間前」が
// 普通に起きる。primary で確かめる。
func TestPruneDangling_KeepsRowsPresentOnPrimary(t *testing.T) {
	s, client := newPruneTestService(t)
	key := streamKey("a1")
	replicaLag, reallyGone := "n_lag", "n_gone"
	require.NoError(t, client.ZAdd(context.Background(), key, redisZ(replicaLag), redisZ(reallyGone)).Err())
	// どちらもレプリカからは引けなかった (notes に無い) が、primary には
	// replicaLag のほうが在る。
	s.SetPrimaryNoteExistence(&fakePrimary{existing: map[string]bool{replicaLag: true}})

	s.PruneDangling(context.Background(), "a1", []string{replicaLag, reallyGone}, nil)

	ids, err := client.ZRange(context.Background(), key, 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{replicaLag}, ids,
		"primary に在る行は消さない (複製遅延で引けなかっただけ)")
}

// TestPruneDangling_FailSafe は、確かめられないときに何もしないことを固定する。
func TestPruneDangling_FailSafe(t *testing.T) {
	t.Run("未配線なら prune しない", func(t *testing.T) {
		s, client := newPruneTestService(t)
		key := streamKey("a1")
		require.NoError(t, client.ZAdd(context.Background(), key, redisZ("n1")).Err())

		// **配線漏れは警告に出す。** 未配線だと prune が丸ごと no-op になるが、
		// fail-safe なので「何も起きない」が正常系と区別できない。router から
		// SetPrimaryNoteExistence を消しても全テストが緑になる状態だったので、
		// ログだけがその変更に気付く手掛かりになる (#2719 review M-4)。
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		s.PruneDangling(context.Background(), "a1", []string{"n1"}, nil)

		ids, _ := client.ZRange(context.Background(), key, 0, -1).Result()
		assert.Equal(t, []string{"n1"}, ids)
		assert.Contains(t, buf.String(), "primary note existence check is not wired",
			"配線漏れが警告に出る")
	})

	t.Run("問い合わせが失敗したら prune しない", func(t *testing.T) {
		s, client := newPruneTestService(t)
		key := streamKey("a1")
		require.NoError(t, client.ZAdd(context.Background(), key, redisZ("n1")).Err())
		s.SetPrimaryNoteExistence(&fakePrimary{err: assert.AnError})

		// **理由が warn に出ることまで固定する。** PruneDangling は
		// confirmMissingOnPrimary の error を握り潰すので、ログが唯一の
		// 手掛かりになる。ここを固定しないと warn を消しても誰も気付かない。
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		s.PruneDangling(context.Background(), "a1", []string{"n1"}, nil)

		ids, _ := client.ZRange(context.Background(), key, 0, -1).Result()
		assert.Equal(t, []string{"n1"}, ids)
		assert.Contains(t, buf.String(), "primary existence check failed",
			"確認の失敗が warn に出る")
	})
}
