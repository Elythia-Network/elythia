package emojiimport_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// journalingEmojiRepo records the order of the calls the importer makes so the
// test can assert *when* the decoration cache is dropped.
type journalingEmojiRepo struct {
	*testutil.MockEmojiRepository
	journal *[]string
}

func (j *journalingEmojiRepo) Create(e *model.Emoji) error {
	*j.journal = append(*j.journal, "create")
	return j.MockEmojiRepository.Create(e)
}

func (j *journalingEmojiRepo) Delete(id string) error {
	*j.journal = append(*j.journal, "delete")
	return j.MockEmojiRepository.Delete(id)
}

type journalingCache struct {
	journal *[]string
}

func (j *journalingCache) Invalidate() { *j.journal = append(*j.journal, "invalidate") }

// **キャッシュを捨てるのは行を書き換えた「後」でなければならない** (#2975)。
//
// 先に捨てると、その窓で並行する `PackUserLite` が取り込み前の状態を読んで
// 30 秒キャッシュし、**取り込んだばかりの絵文字が `avatarDecorations` から
// 落ちる** — この invalidate が防ごうとしている #2258 の症状そのものが、
// invalidate を入れたまま再現する。
//
// `defer` で書いてあることを「位置」ではなく**呼び出し順**で固定する。
// 静的なゲート (`TestEmojiMutationsDropDecorationCache`) は存在しか見ないので、
// 関数の先頭へ動かす変異はそちらでは捕まらない。
func TestImport_InvalidatesDecorationCacheAfterWriting(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}})

	deps, _, repo, _ := newDeps(t, body)
	// 置き換え経路も通す (delete → create の両方が invalidate より前に来ること)。
	require.NoError(t, repo.Create(&model.Emoji{ID: "old", Name: "smile"}))

	journal := []string{}
	deps.EmojiRepo = &journalingEmojiRepo{MockEmojiRepository: repo, journal: &journal}
	deps.DecorationCache = &journalingCache{journal: &journal}
	// 事前投入の Create は journal に載せない (wrapper を差し替える前に実行済み)。

	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)

	require.Equal(t, []string{"delete", "create", "invalidate"}, journal,
		"キャッシュを捨てるのは行を書き換えた後でなければならない")
}

// 新規追加 (置き換えではない) でも捨てる。
func TestImport_InvalidatesDecorationCacheOnFreshAdd(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "new.png", "downloaded": true, "emoji": map[string]any{"name": "brandnew"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"new.png", img}})

	deps, _, repo, _ := newDeps(t, body)
	journal := []string{}
	deps.EmojiRepo = &journalingEmojiRepo{MockEmojiRepository: repo, journal: &journal}
	deps.DecorationCache = &journalingCache{journal: &journal}

	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)

	assert.Equal(t, []string{"create", "invalidate"}, journal)
}

// 未配線でも落ちない (unit test は毎回 stub を刺さない)。
func TestImport_DecorationCacheUnwired(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "x.png", "downloaded": true, "emoji": map[string]any{"name": "x"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"x.png", img}})
	deps, _, _, _ := newDeps(t, body)

	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
}
