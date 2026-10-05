package webpush

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/queue"
)

// Service builds Web Push delivery jobs and enqueues them.
// Delivery itself is performed asynchronously by the queue worker; callers
// only observe the enqueue latency.
//
// 本家 PushNotificationService.ts と異なり非同期キュー経由にすることで、
// 通知作成フロー (i.e. note create / follow 等) の主経路を詰まらせないように
// している。
type Service struct {
	enqueuer queue.Enqueuer
}

// NewService returns a new webpush.Service bound to the queue enqueuer.
func NewService(enqueuer queue.Enqueuer) *Service {
	return &Service{enqueuer: enqueuer}
}

// PushNotification enqueues a `notification` push to userID.
// body はnotificationエンティティ相当のmapを想定する。
// 本家の PushNotificationService.pushNotification('notification', packed) に対応する。
func (s *Service) PushNotification(userID string, body map[string]any) {
	s.push(userID, TypeNotification, body)
}

// PushUnreadAntennaNote enqueues an `unreadAntennaNote` push.
// body は { antenna: {id, name}, note: PackedNote } 相当のmap。
func (s *Service) PushUnreadAntennaNote(userID string, body map[string]any) {
	s.push(userID, TypeUnreadAntennaNote, body)
}

// PushReadAllNotifications enqueues a `readAllNotifications` push.
// フロントで通知トーストを閉じる用途。body は空 (undefined) でよい。
func (s *Service) PushReadAllNotifications(userID string) {
	s.push(userID, TypeReadAllNotifications, nil)
}

// PushNewChatMessage enqueues a `newChatMessage` push.
// body は packed ChatMessage。
func (s *Service) PushNewChatMessage(userID string, body map[string]any) {
	s.push(userID, TypeNewChatMessage, body)
}

func (s *Service) push(userID, pushType string, body map[string]any) {
	if s == nil || s.enqueuer == nil || userID == "" {
		return
	}
	truncated := TruncateBody(pushType, body)
	var raw json.RawMessage
	if truncated != nil {
		b, err := fitPayload(truncated)
		if err != nil {
			slog.Warn("webpush: marshal body failed", "type", pushType, "err", err)
			return
		}
		raw = b
	}
	payload := queue.WebPushPayload{UserID: userID, Type: pushType, Body: raw}
	if err := s.enqueuer.EnqueueWebPush(context.Background(), payload); err != nil {
		slog.Warn("webpush: enqueue failed", "type", pushType, "user", userID, "err", err)
	}
}

// maxPushBodyBytes bounds the JSON body handed to the queue.
//
// **Web Push には実質 4 KB の上限がある。** RFC 8291 の aes128gcm は既定の
// record size 4096 で、内訳は header 86 B + 平文 + tag 16 B + padding 区切り
// 1 B。したがって平文は 3993 B までで、**webpush-go はこれを超えると送信前に
// ErrMaxPadExceeded を返す** (`pad()` の maxPadLen = 4080 - 86)。push service に
// 到達しないので、超えた通知は 413 ですらなく**単に配信されない**
// (upstream の node 実装は分割して送るので、あちらは push service が 413 を返す)。
//
// ここで見るのは body だけなので、worker が被せる envelope
// (`{"type":…,"body":…,"userId":…,"dateTime":…}`) のぶんを引いてある。envelope は
// 実測で `56 + len(type) + len(userId)` = 84-92 B (ULID の userId と
// `readAllNotifications` の組で最大 102 B。ただしこの型は body が nil で
// fitPayload を通らない) なので 90 B 以上の余裕がある。
const maxPushBodyBytes = 3800

// pushTextTailKeep is how many bytes of the summary tail are preserved when the
// text has to be cut.
//
// **要約の末尾に情報が付く。** `notesummary.Get` は本文の後ろに `(📎N)` /
// `(📊)` / `\n\nRE: …` / `\n\nRN: …` を足すので、末尾から削ると #2737 で
// 出せるようにした添付件数がまさに消える。先頭側を削って末尾を残す。
// マーカーだけなら最大 34 B (`(📎16)` 9 + `(📊)` 7 + `RE: ...` 9 + `RN: ...` 9)
// なので 96 で足りる。
//
// **reply / renote が hydrate されている場合は、どんな定数でも足りない。**
// `notesummary.Get` はそのとき `Get(reply)` / `Get(renote)` を再帰で連結する
// ので、自分の `(📎N)` は文字列の**途中**に来る (最大でノート 3 本ぶん)。その
// 形では末尾を残しても親ノートの要約が残るだけで、自分の添付件数は落ちうる。
// #2737 が対象にした「本文が無く画像だけ」の通知は要約が `(📎1)` だけなので
// 切り詰め自体が起きず、影響しない。
const pushTextTailKeep = 96

// fitPayload marshals body, shrinking it when it would not fit in a Web Push
// payload. Returns the marshaled JSON.
//
// **添付を載せると簡単に超える。** 実測 (URL / blurhash / comment を現実的な
// 長さにした画像) で、packed drive file は 1 件 816 B (JSON 配列にすると 818 B)。
// 4 件で 3.3 KB、16 件 (note の添付上限) で 13.1 KB になる。upstream はサイズを見ずに送るので
// (node の http_ece は rs を超える平文を複数レコードに分割するだけで拒否しない)、
// 添付の多い note の push は push service に 413 で拒否される。
//
// 縮め方は「情報量の少ないものから、**積み上げて**」:
//
//  1. `note.files` — 件数は既に `note.text` の要約 `(📎N)` に入っている
//     (TruncateBody が先に走る) ので、落としても通知の意味は保たれる
//  2. **1 に加えて** `note.text` の先頭側 — 本文は最大 3000 文字 (日本語で
//     9000 B) 入るので、files を落としただけでは収まらない
//  3. `note` ごと — **最後の手段**。sw.js の composeNotification は
//     `data.body.note` を無条件に参照する (`noteId` は一度も読まない) ので、
//     落とすと TypeError で通知はブラウザ既定の汎用表示に化ける。それでも
//     「配信されない」より情報が残るという判断
//
// **段を排他にしないこと。** 2 を「元の note」から始めると、files を落とせば
// 収まる余地があっても text 側の見積りが負になって全部落ちる。実測で
// 「写真 5 枚 + 日本語 1300 文字」(どちらも上限の内側) が note ごと落ちた。
func fitPayload(body map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil || len(raw) <= maxPushBodyBytes {
		return raw, err
	}

	if note, ok := body["note"].(map[string]any); ok && note != nil {
		work := make(map[string]any, len(note))
		maps.Copy(work, note)

		if _, has := work["files"]; has {
			// 元から files を持たない body に空配列を生やさない。
			//
			// **これは意図の表明であって、振る舞いは変わらない。** files キーが
			// 無ければ空配列を足しても縮まず、どのみち採用されない (変異させても
			// テストは落ちない)。
			work["files"] = []any{}
			if out, ok := marshalWithNote(body, work); ok {
				slog.Debug("webpush: dropped note.files to fit payload", "was", len(raw), "now", len(out))
				return out, nil
			}
		}
		if text, _ := work["text"].(string); text != "" {
			if out, ok := shrinkNoteText(body, work, text); ok {
				slog.Debug("webpush: truncated note.text to fit payload", "was", len(raw), "now", len(out))
				return out, nil
			}
		}
	}

	if _, ok := body["note"]; ok {
		withoutNote := make(map[string]any, len(body))
		maps.Copy(withoutNote, body)
		delete(withoutNote, "note")
		if out, err2 := json.Marshal(withoutNote); err2 == nil && len(out) <= maxPushBodyBytes {
			slog.Warn("webpush: dropped note detail to fit payload; sw.js will fall back to a generic notification",
				"bytes", len(out))
			return out, nil
		}
	}

	slog.Warn("webpush: payload exceeds the Web Push limit and could not be shrunk",
		"bytes", len(raw), "limit", maxPushBodyBytes)
	return raw, nil
}

// shrinkNoteText finds the longest cut of text that still fits, mutating work.
//
// **二分探索で実際に marshal して確かめること。** 「超過ぶんだけ削る」という
// byte 数の見積りは JSON escape でずれる。`<` / `&` / 制御文字は 1 文字が
// `\u003c` の 6 B に膨らむので、見積りだと budget が負になって本文が丸ごと
// 消える (実測で `<` を 3000 個並べた本文が空になった)。
func shrinkNoteText(body, work map[string]any, text string) ([]byte, bool) {
	lo, hi := 0, len(text)
	best := -1
	var bestRaw []byte
	for lo <= hi {
		mid := (lo + hi) / 2
		work["text"] = cutKeepingTail(text, mid)
		if out, ok := marshalWithNote(body, work); ok {
			best, bestRaw = mid, out
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best < 0 {
		return nil, false
	}
	work["text"] = cutKeepingTail(text, best)
	return bestRaw, true
}

// marshalWithNote marshals body with note replaced, reporting whether the
// result fits.
func marshalWithNote(body, note map[string]any) ([]byte, bool) {
	next := make(map[string]any, len(body))
	maps.Copy(next, body)
	next["note"] = note
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > maxPushBodyBytes {
		return nil, false
	}
	return raw, true
}

// cutKeepingTail shortens s to at most budget bytes, keeping both the head and
// the last pushTextTailKeep bytes. Cuts land on rune boundaries.
func cutKeepingTail(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(s) <= budget {
		return s
	}
	const ellipsis = "\u2026"
	tail := pushTextTailKeep
	if tail > budget/2 {
		tail = budget / 2
	}
	head := budget - tail - len(ellipsis)
	if head <= 0 {
		return truncateRunesFromEnd(s, budget)
	}
	return truncateRunes(s, head) + ellipsis + truncateRunesFromEnd(s, tail)
}

// truncateRunes returns the longest rune-aligned prefix of s within n bytes.
//
// n <= 0 のガードは**防御**。現在の cutKeepingTail からは負の n で呼ばれないので
// 外してもテストは落ちないが、budget 式や pushTextTailKeep を触ると即 panic
// (slice bounds out of range) になる位置なので置いてある。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncateRunesFromEnd returns the longest rune-aligned suffix of s within n bytes.
func truncateRunesFromEnd(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
