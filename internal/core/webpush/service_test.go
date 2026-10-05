package webpush_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEnqueuer captures EnqueueWebPush calls for assertion.
type fakeEnqueuer struct {
	calls []queue.WebPushPayload
	err   error
}

func (f *fakeEnqueuer) EnqueueDeliver(_ queue.DeliverPayload, _ ...driver.EnqueueOption) error {
	return nil
}
func (f *fakeEnqueuer) EnqueueExport(_ queue.ExportPayload) error { return nil }
func (f *fakeEnqueuer) EnqueueImport(_ queue.ImportPayload) error { return nil }
func (f *fakeEnqueuer) EnqueueWebPush(_ context.Context, payload queue.WebPushPayload) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, payload)
	return nil
}
func (f *fakeEnqueuer) EnqueueUserWebhook(_ context.Context, _ queue.WebhookPayload) error {
	return nil
}
func (f *fakeEnqueuer) EnqueueSystemWebhook(_ context.Context, _ queue.WebhookPayload) error {
	return nil
}
func (f *fakeEnqueuer) EnqueueInbox(_ context.Context, _ queue.InboxPayload) error { return nil }
func (f *fakeEnqueuer) EnqueuePostScheduledNote(_ queue.PostScheduledNotePayload, _ ...driver.EnqueueOption) error {
	return nil
}
func (f *fakeEnqueuer) ClearScheduledNote(_ string) error { return nil }
func (f *fakeEnqueuer) Close() error                      { return nil }

func TestService_PushNotification_Enqueues(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := webpush.NewService(enq)
	svc.PushNotification("u1", map[string]any{
		"type": "mention",
		"note": map[string]any{"text": "hi", "cw": "spoil"},
	})
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "u1", enq.calls[0].UserID)
	assert.Equal(t, webpush.TypeNotification, enq.calls[0].Type)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
	note := decoded["note"].(map[string]any)
	// cw が優先されて summary になる
	assert.Equal(t, "spoil", note["text"])
}

func TestService_PushUnreadAntennaNote(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := webpush.NewService(enq)
	svc.PushUnreadAntennaNote("u1", map[string]any{
		"antenna": map[string]any{"id": "a1", "name": "ant"},
		"note":    map[string]any{"text": "t"},
	})
	require.Len(t, enq.calls, 1)
	assert.Equal(t, webpush.TypeUnreadAntennaNote, enq.calls[0].Type)
}

func TestService_PushReadAllNotifications(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := webpush.NewService(enq)
	svc.PushReadAllNotifications("u1")
	require.Len(t, enq.calls, 1)
	assert.Equal(t, webpush.TypeReadAllNotifications, enq.calls[0].Type)
	assert.Empty(t, enq.calls[0].Body)
}

func TestService_PushNewChatMessage(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := webpush.NewService(enq)
	svc.PushNewChatMessage("u1", map[string]any{"id": "m1", "text": "hi"})
	require.Len(t, enq.calls, 1)
	assert.Equal(t, webpush.TypeNewChatMessage, enq.calls[0].Type)
}

func TestService_NilReceiverNoPanic(t *testing.T) {
	var svc *webpush.Service
	svc.PushNotification("u1", nil)
	svc.PushReadAllNotifications("u1")
}

func TestService_EmptyUserIDSkipped(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := webpush.NewService(enq)
	svc.PushNotification("", map[string]any{"type": "x"})
	assert.Empty(t, enq.calls)
}

func TestService_EnqueueErrorSilentlySwallowed(t *testing.T) {
	enq := &fakeEnqueuer{err: errors.New("boom")}
	svc := webpush.NewService(enq)
	// エラーは silent に log へ。panic しないことを確認。
	svc.PushNotification("u1", map[string]any{"type": "t"})
}

// realisticFile returns a packed drive file of a size seen in practice
// (長い URL / blurhash / 代替テキスト付きの画像)。約 800 B。
func realisticFile(i int) map[string]any {
	return map[string]any{
		"id":           fmt.Sprintf("a1b2c3d4e5f6g7h%d", i),
		"createdAt":    "2026-08-30T04:05:06.000Z",
		"name":         "IMG_20260830_123456_very_long_original_filename.jpeg",
		"type":         "image/jpeg",
		"md5":          "0123456789abcdef0123456789abcdef",
		"size":         3456789,
		"blurhash":     "eEHV6nWB2yk8pyo0adR*.7kCMdnjSgQ,f6ayj[ayfkayfQay",
		"comment":      "代替テキストがそこそこ長い場合を想定した説明文です",
		"url":          "https://media.example.com/files/webpublic-0123456789abcdef-0123-4567-89ab-cdef01234567.webp",
		"thumbnailUrl": "https://media.example.com/files/thumbnail-0123456789abcdef-0123-4567-89ab-cdef01234567.webp",
		"webpublicUrl": "https://media.example.com/files/webpublic-0123456789abcdef-0123-4567-89ab-cdef01234567.webp",
		"folderId":     "a1b2c3d4e5f6g7h9",
		"userId":       "a1b2c3d4e5f6g7h8",
		"folder":       nil,
		"user":         nil,
		"properties":   map[string]any{"width": 4032, "height": 3024},
		"isSensitive":  false,
	}
}

// #2737: Web Push には実質 4 KB の上限があり、超えると webpush-go が送信前に
// ErrMaxPadExceeded を返して**通知が丸ごと届かない**。添付を載せるようにしたので、
// note の添付上限 (16 件) でも収まることを固定する。
func TestService_PushNotification_FitsWebPushLimit(t *testing.T) {
	for _, n := range []int{1, 4, 16} {
		t.Run(fmt.Sprintf("files=%d", n), func(t *testing.T) {
			files := make([]any, 0, n)
			for i := 0; i < n; i++ {
				files = append(files, realisticFile(i))
			}
			enq := &fakeEnqueuer{}
			webpush.NewService(enq).PushNotification("a1b2c3d4e5f6g7h8", map[string]any{
				"id": "a1b2c3d4e5f6g7hz", "type": "mention",
				"note": map[string]any{
					"id": "a1b2c3d4e5f6g7hy", "text": "", "files": files,
					"fileIds": []any{"a1b2c3d4e5f6g7h0"},
				},
			})
			require.Len(t, enq.calls, 1)
			assert.LessOrEqual(t, len(enq.calls[0].Body), 3800,
				"payload が上限を超えると webpush-go が送信前に弾く")

			// **要約は落とさない。** files を落としても `(📎N)` は残る。
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
			note, _ := decoded["note"].(map[string]any)
			require.NotNil(t, note, "note ごと落ちるほど縮めない")
			assert.Equal(t, fmt.Sprintf("(\U0001F4CE%d)", n), note["text"])
		})
	}
}

// 上限に収まる payload は一切削らない (files はそのまま届く)。
func TestService_PushNotification_KeepsFilesWhenSmall(t *testing.T) {
	enq := &fakeEnqueuer{}
	webpush.NewService(enq).PushNotification("u1", map[string]any{
		"type": "mention",
		"note": map[string]any{"text": "hi", "files": []any{realisticFile(0)}},
	})
	require.Len(t, enq.calls, 1)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
	note, _ := decoded["note"].(map[string]any)
	files, _ := note["files"].([]any)
	assert.Len(t, files, 1, "収まるなら files は削らない")
}

// files を落としても収まらない場合は本文を切り詰める。**note は落とさない** —
// sw.js の composeNotification は `data.body.note` を無条件に参照し、`noteId` は
// 一度も読まないので、落とすと通知がブラウザ既定の汎用表示に化ける。
func TestService_PushNotification_TruncatesTextInsteadOfDroppingNote(t *testing.T) {
	enq := &fakeEnqueuer{}
	webpush.NewService(enq).PushNotification("u1", map[string]any{
		"type": "mention", "noteId": "a1b2c3d4e5f6g7hy",
		"note": map[string]any{
			"id": "a1b2c3d4e5f6g7hy", "text": strings.Repeat("あ", 3000),
		},
	})
	require.Len(t, enq.calls, 1)
	assert.LessOrEqual(t, len(enq.calls[0].Body), 3800)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
	note, _ := decoded["note"].(map[string]any)
	require.NotNil(t, note, "note を落とすと sw.js が描画できない")
	text, _ := note["text"].(string)
	assert.NotEmpty(t, text)
	assert.Less(t, len(text), 3000*3, "本文は切り詰められる")
}

// **末尾を残して切ること。** 要約は `本文 + (📎N)` の順で組み立てられるので、
// 末尾から削ると #2737 で出せるようにした添付件数がまさに消える。
func TestService_PushNotification_TruncationKeepsSummaryTail(t *testing.T) {
	files := make([]any, 0, 2)
	for i := 0; i < 2; i++ {
		files = append(files, realisticFile(i))
	}
	enq := &fakeEnqueuer{}
	webpush.NewService(enq).PushNotification("u1", map[string]any{
		"type": "mention",
		"note": map[string]any{
			"id": "a1b2c3d4e5f6g7hy", "text": strings.Repeat("あ", 3000), "files": files,
		},
	})
	require.Len(t, enq.calls, 1)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
	note, _ := decoded["note"].(map[string]any)
	require.NotNil(t, note)
	text, _ := note["text"].(string)
	assert.True(t, strings.HasSuffix(text, "(\U0001F4CE2)"),
		"末尾の (📎N) が残ること: %q", text[max(0, len(text)-40):])
	assert.Contains(t, text, "\u2026", "先頭側を削った印を残す")
}

// 切り詰めは rune 境界で行う (壊れた UTF-8 を送らない)。
//
// **オフセットをずらして複数回試すこと。** 3 バイト文字だけを並べると切り出し
// 位置がたまたま境界に揃い、境界合わせを外す変異でも通ってしまう。ASCII を
// 前後に混ぜて 3 で割り切れない位置を作る。
func TestService_PushNotification_TruncationIsRuneSafe(t *testing.T) {
	for pad := 0; pad < 3; pad++ {
		for _, suffix := range []string{"", "x", "xy"} {
			t.Run(fmt.Sprintf("pad=%d suffix=%q", pad, suffix), func(t *testing.T) {
				text := strings.Repeat("x", pad) + strings.Repeat("あ", 3000) + suffix
				enq := &fakeEnqueuer{}
				webpush.NewService(enq).PushNotification("u1", map[string]any{
					"type": "mention",
					"note": map[string]any{"id": "n1", "text": text},
				})
				require.Len(t, enq.calls, 1)
				assert.True(t, utf8.Valid(enq.calls[0].Body), "payload 自体が有効な UTF-8")

				var decoded map[string]any
				require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
				note, _ := decoded["note"].(map[string]any)
				got, _ := note["text"].(string)
				assert.True(t, utf8.ValidString(got))
				assert.NotContains(t, got, "\uFFFD", "壊れた rune を置換文字にして送らない")
			})
		}
	}
}

// 本文を空にしても収まらない場合の最後の手段。note ごと落とすので sw.js は
// 汎用表示に化けるが、配信そのものが失敗するよりは情報が残る。
func TestService_PushNotification_DropsNoteAsLastResort(t *testing.T) {
	// text 以外だけで上限を超える note を作る (mentions が膨らんだ形)。
	mentions := make([]any, 0, 300)
	for i := 0; i < 300; i++ {
		mentions = append(mentions, fmt.Sprintf("a1b2c3d4e5f6g7h%03d", i))
	}
	enq := &fakeEnqueuer{}
	webpush.NewService(enq).PushNotification("u1", map[string]any{
		"type": "mention", "noteId": "a1b2c3d4e5f6g7hy",
		"note": map[string]any{"id": "a1b2c3d4e5f6g7hy", "text": "hi", "mentions": mentions},
	})
	require.Len(t, enq.calls, 1)
	assert.LessOrEqual(t, len(enq.calls[0].Body), 3800)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
	assert.Nil(t, decoded["note"])
	assert.Equal(t, "a1b2c3d4e5f6g7hy", decoded["noteId"], "noteId は残す")
}

// note を持たない body (newChatMessage) は縮めようがないので、そのまま渡す。
// **ここで捨てない。** 配信は webpush-go 側で失敗するが、握り潰して無音にする
// より、warn を出して失敗を観測できるほうがよい。
func TestService_PushNewChatMessage_OversizedIsPassedThrough(t *testing.T) {
	enq := &fakeEnqueuer{}
	webpush.NewService(enq).PushNewChatMessage("u1", map[string]any{
		"id": "m1", "text": strings.Repeat("あ", 3000),
	})
	require.Len(t, enq.calls, 1)
	assert.Greater(t, len(enq.calls[0].Body), 3800, "縮められないものはそのまま")
}

// 元の note が files を持たない場合、縮小で空配列を生やさない。
//
// **早期 return を守るテストではない。** files キーが無ければ空配列を足しても
// 縮まないので、ガードを外しても結果は同じ。固定しているのは結果の shape。
func TestService_PushNotification_DoesNotInventFilesKey(t *testing.T) {
	enq := &fakeEnqueuer{}
	webpush.NewService(enq).PushNotification("u1", map[string]any{
		"type": "mention",
		"note": map[string]any{"id": "n1", "text": strings.Repeat("あ", 3000)},
	})
	require.Len(t, enq.calls, 1)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
	note, _ := decoded["note"].(map[string]any)
	_, has := note["files"]
	assert.False(t, has, "元から無い files キーを生やさない")
}

// **段は積み上げること** (#2737 レビュー 2 巡目 H-1)。files を落としたうえで
// 本文を切る、を試さないと「写真 5 枚 + 長文キャプション」で note ごと落ちる。
// どちらも上限 (files 16 / text 3000 文字) の内側で、実運用で普通に起きる。
func TestService_PushNotification_ComposesShrinkSteps(t *testing.T) {
	cases := []struct{ files, chars int }{
		{4, 1300}, {5, 500}, {5, 1300}, {16, 1300}, {16, 3000},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("files=%d chars=%d", tc.files, tc.chars), func(t *testing.T) {
			files := make([]any, 0, tc.files)
			for i := 0; i < tc.files; i++ {
				files = append(files, realisticFile(i))
			}
			enq := &fakeEnqueuer{}
			webpush.NewService(enq).PushNotification("u1", map[string]any{
				"type": "mention", "noteId": "a1b2c3d4e5f6g7hy",
				"note": map[string]any{
					"id": "a1b2c3d4e5f6g7hy", "text": strings.Repeat("あ", tc.chars), "files": files,
				},
			})
			require.Len(t, enq.calls, 1)
			assert.LessOrEqual(t, len(enq.calls[0].Body), 3800)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
			note, _ := decoded["note"].(map[string]any)
			require.NotNil(t, note, "note ごと落とすと sw.js が描画できない")
			text, _ := note["text"].(string)
			assert.NotEmpty(t, text, "本文が丸ごと消えるほど縮めない")
		})
	}
}

// JSON escape で膨らむ本文でも、残せるだけ残すこと (#2737 レビュー 2 巡目 M-2)。
// `<` / `&` / 制御文字は 1 文字が `<` の 6 B になるので、byte 数の見積りで
// 削ると budget が負になって本文が丸ごと消える。
func TestService_PushNotification_TruncationHandlesJSONEscaping(t *testing.T) {
	for _, ch := range []string{"<", "&", "\"", "\\", "\x01"} {
		t.Run(fmt.Sprintf("%q", ch), func(t *testing.T) {
			enq := &fakeEnqueuer{}
			webpush.NewService(enq).PushNotification("u1", map[string]any{
				"type": "mention",
				"note": map[string]any{"id": "n1", "text": strings.Repeat(ch, 3000)},
			})
			require.Len(t, enq.calls, 1)
			assert.LessOrEqual(t, len(enq.calls[0].Body), 3800)

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
			note, _ := decoded["note"].(map[string]any)
			text, _ := note["text"].(string)
			assert.Greater(t, len(text), 300, "escape が膨らむ本文でも残せるだけ残す")
		})
	}
}

// 末尾に残すのは 1 マーカー分では足りない。`(📎16) (📊)` が丸ごと残ること。
func TestService_PushNotification_TruncationKeepsMultipleMarkers(t *testing.T) {
	tail := " (\U0001F4CE16) (\U0001F4CA)"
	enq := &fakeEnqueuer{}
	webpush.NewService(enq).PushNotification("u1", map[string]any{
		"type": "mention",
		"note": map[string]any{"id": "n1", "text": strings.Repeat("あ", 3000) + tail},
	})
	require.Len(t, enq.calls, 1)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &decoded))
	note, _ := decoded["note"].(map[string]any)
	text, _ := note["text"].(string)
	assert.True(t, strings.HasSuffix(text, tail), "末尾のマーカーがまとめて残ること: %q", text[max(0, len(text)-40):])
}
