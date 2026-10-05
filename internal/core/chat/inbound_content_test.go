package chat_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub/mfm"
	"github.com/elythia-network/elythia/internal/model"
)

// chatMessageTextMaxRunes (internal/core/chat) と同じ値。core 側は unexported
// なので定数をここにも置く。ずれても下の truncation test がそのまま落ちる
// (期待値がこの定数で書かれているため)。
const testChatMessageTextMaxRunes = 4096

// renderChatContent renders text the way the outbound renderer does
// (`mfm.Parse` -> `mfm.ToHTML`) so the round-trip tests stay honest even if the
// sender side changes shape.
func renderChatContent(text string) string {
	return mfm.ToHTML(mfm.Parse(text), "remote.example")
}

// 連合で届く 1-on-1 chat の `content` は HTML。MFM に戻してから保存しないと
// `&lt;` / `&amp;` がそのまま本文として表示される (frontend は MFM として描画する)。
func TestCreateMessageViaAP_DecodesHTMLContentToMFM(t *testing.T) {
	svc, repo, _ := newSvc(t)
	sender := &model.User{ID: "alice"}

	uri := "https://remote.example/chat/messages/m1"
	_, err := svc.CreateMessageViaAP(context.Background(), uri, sender, "bob", "<p>a &lt; b &amp; c</p>", "")
	require.NoError(t, err)

	stored, ferr := repo.FindMessageByURI(uri)
	require.NoError(t, ferr)
	require.NotNil(t, stored.Text)
	assert.Equal(t, "a < b & c", *stored.Text)
	assert.NotContains(t, *stored.Text, "&amp;", "HTML entity を本文に残さない")
	assert.NotContains(t, *stored.Text, "<p>", "HTML tag を本文に残さない")
}

// group (room) chat も同じ扱い。
func TestCreateRoomMessageViaAP_DecodesHTMLContentToMFM(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "rmt")))
	sender := &model.User{ID: "rmt"}

	uri := "https://remote.example/chat/messages/m1"
	require.NoError(t, svc.CreateRoomMessageViaAP(uri, sender, remoteRoomURI("room1"), "<p>a &lt; b &amp; c</p>", ""))

	stored, ferr := repo.FindMessageByURI(uri)
	require.NoError(t, ferr)
	require.NotNil(t, stored.Text)
	assert.Equal(t, "a < b & c", *stored.Text)
}

// 送信側 (renderer) が出す HTML を受け取ると元の MFM に戻ること。
// `<script>` や `&` を含む本文が mk-go 同士の往復で壊れないことを固定する。
func TestCreateMessageViaAP_RoundTripsRenderedText(t *testing.T) {
	cases := []string{
		"a < b & c",
		"<script>alert(1)</script> は文字列として残る",
		"5 > 3 && 2 < 4",
		"line1\nline2",
		"**bold** text",
		"plain",
	}
	for i, text := range cases {
		svc, repo, _ := newSvc(t)
		sender := &model.User{ID: "alice"}
		uri := "https://remote.example/chat/messages/rt" + string(rune('a'+i))

		_, err := svc.CreateMessageViaAP(context.Background(), uri, sender, "bob", renderChatContent(text), "")
		require.NoError(t, err)

		stored, ferr := repo.FindMessageByURI(uri)
		require.NoError(t, ferr)
		require.NotNil(t, stored.Text, "text=%q", text)
		assert.Equal(t, text, *stored.Text, "renderer が出した HTML は元の MFM に戻ること")
	}
}

// room 経路でも往復すること (1-on-1 だけ直して片側が残る形を防ぐ)。
func TestCreateRoomMessageViaAP_RoundTripsRenderedText(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "rmt")))
	sender := &model.User{ID: "rmt"}
	text := "<script>alert(1)</script> & 5 > 3"
	uri := "https://remote.example/chat/messages/m2"

	require.NoError(t, svc.CreateRoomMessageViaAP(uri, sender, remoteRoomURI("room1"), renderChatContent(text), ""))

	stored, ferr := repo.FindMessageByURI(uri)
	require.NoError(t, ferr)
	require.NotNil(t, stored.Text)
	assert.Equal(t, text, *stored.Text)
}

// **変換してから切る。** 先に切ると tag の途中で切れた壊れた HTML を parser に
// 渡すことになり、列の上限ぶんの本文も残らない。
func TestCreateMessageViaAP_ConvertsBeforeTruncating(t *testing.T) {
	svc, repo, _ := newSvc(t)
	sender := &model.User{ID: "alice"}
	body := strings.Repeat("a", testChatMessageTextMaxRunes+500)
	uri := "https://remote.example/chat/messages/long"

	_, err := svc.CreateMessageViaAP(context.Background(), uri, sender, "bob", "<p>"+body+"</p>", "")
	require.NoError(t, err)

	stored, ferr := repo.FindMessageByURI(uri)
	require.NoError(t, ferr)
	require.NotNil(t, stored.Text)
	assert.Equal(t, testChatMessageTextMaxRunes, utf8.RuneCountInString(*stored.Text),
		"列の上限ぶんの本文が残ること (先に切ると tag の分だけ短くなる)")
	assert.NotContains(t, *stored.Text, "<", "壊れた HTML の断片を残さない")
}

// 変換の結果が空になったら列は NULL のままにする (生値が空だったときと同じ形)。
func TestCreateMessageViaAP_EmptyAfterConversionKeepsNullText(t *testing.T) {
	svc, repo, _ := newSvc(t)
	sender := &model.User{ID: "alice"}
	uri := "https://remote.example/chat/messages/empty"

	_, err := svc.CreateMessageViaAP(context.Background(), uri, sender, "bob", "<p></p>", "")
	require.NoError(t, err)

	stored, ferr := repo.FindMessageByURI(uri)
	require.NoError(t, ferr)
	assert.Nil(t, stored.Text, "空になったら text は NULL")
}

// **ローカル発の本文は変換しない。** local API が受け取るのは MFM そのもので、
// HTML として解釈すると利用者が打った `<` や `&` が消える。
func TestCreateMessageToUser_KeepsLocalTextVerbatim(t *testing.T) {
	svc, repo, _ := newSvc(t)
	text := "a < b & <p>tag</p>"

	msg, err := svc.CreateMessageToUser(context.Background(), "alice", "bob", text, "")
	require.NoError(t, err)
	require.NotNil(t, msg.Text)
	assert.Equal(t, text, *msg.Text)

	stored, ferr := repo.FindMessageByID(msg.ID)
	require.NoError(t, ferr)
	require.NotNil(t, stored.Text)
	assert.Equal(t, text, *stored.Text)
}

// room へのローカル投稿も同じ (AP 経路だけを変換していること)。
func TestCreateMessageToRoom_KeepsLocalTextVerbatim(t *testing.T) {
	svc, repo, _ := newSvc(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "alice")))
	text := "a < b & <p>tag</p>"

	msg, err := svc.CreateMessageToRoom(context.Background(), "alice", "room1", text, "")
	require.NoError(t, err)
	require.NotNil(t, msg.Text)
	assert.Equal(t, text, *msg.Text)
}

// **原文があれば HTML から戻さない。** `mfm.Parse` -> `ToHTML` -> `FromHTML` は
// 往復で情報が落ちる (装飾・色・`<center>`・引用の改行・数式が失われ、絵文字の
// 前後にゼロ幅文字が入る)。送信側が `content` をエスケープするようになった分、
// 受信側で原文を使わないと mk-go 間の再現性がむしろ下がる。
func TestCreateMessageViaAP_PrefersMFMSource(t *testing.T) {
	// 往復で壊れることが分かっている MFM を使う。
	for _, text := range []string{"$[shake ゆれる]", "$[fg.color=f00 あか]", "<center>まんなか</center>"} {
		t.Run(text, func(t *testing.T) {
			svc, repo, _ := newSvc(t)
			sender := &model.User{ID: "alice"}
			uri := "https://remote.example/chat/messages/" + text

			// content は HTML 化されたもの、mfmSource は原文。
			_, err := svc.CreateMessageViaAP(context.Background(), uri, sender, "bob",
				renderChatContent(text), text)
			require.NoError(t, err)

			stored, ferr := repo.FindMessageByURI(uri)
			require.NoError(t, ferr)
			require.NotNil(t, stored.Text)
			assert.Equal(t, text, *stored.Text, "MFM の原文があるのに HTML から戻している")
		})
	}
}

// room も同じ扱い。
func TestCreateRoomMessageViaAP_PrefersMFMSource(t *testing.T) {
	svc, repo := newRoomFedService(t)
	require.NoError(t, repo.CreateRoom(remoteRoomRow("room1", "General", "rmt")))
	sender := &model.User{ID: "rmt"}

	const text = "$[shake ゆれる]"
	uri := "https://remote.example/chat/messages/m-src"
	require.NoError(t, svc.CreateRoomMessageViaAP(uri, sender, remoteRoomURI("room1"), renderChatContent(text), text))

	stored, ferr := repo.FindMessageByURI(uri)
	require.NoError(t, ferr)
	require.NotNil(t, stored.Text)
	assert.Equal(t, text, *stored.Text, "MFM の原文があるのに HTML から戻している")
}

// 原文にも長さの上限と NUL の除去を掛ける。`FromHTML` を通らないので、
// ここで落とさないと PostgreSQL の text 列が 22021 で落ちる。
func TestCreateMessageViaAP_MFMSourceIsTruncatedAndSanitized(t *testing.T) {
	svc, repo, _ := newSvc(t)
	sender := &model.User{ID: "alice"}

	t.Run("長さ", func(t *testing.T) {
		uri := "https://remote.example/chat/messages/long-src"
		long := strings.Repeat("あ", testChatMessageTextMaxRunes+100)
		_, err := svc.CreateMessageViaAP(context.Background(), uri, sender, "bob", "<p>x</p>", long)
		require.NoError(t, err)

		stored, ferr := repo.FindMessageByURI(uri)
		require.NoError(t, ferr)
		require.NotNil(t, stored.Text)
		assert.Equal(t, testChatMessageTextMaxRunes, utf8.RuneCountInString(*stored.Text),
			"原文に長さの上限が掛かっていない")
	})

	t.Run("NUL", func(t *testing.T) {
		uri := "https://remote.example/chat/messages/nul-src"
		_, err := svc.CreateMessageViaAP(context.Background(), uri, sender, "bob", "<p>x</p>", "a\x00b")
		require.NoError(t, err)

		stored, ferr := repo.FindMessageByURI(uri)
		require.NoError(t, ferr)
		require.NotNil(t, stored.Text)
		assert.Equal(t, "ab", *stored.Text, "原文の NUL を落としていない")
	})
}
