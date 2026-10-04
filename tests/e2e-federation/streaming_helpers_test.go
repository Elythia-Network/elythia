// streaming_helpers_test.go: WebSocket /streaming テスト用クライアント (#781)。
//
// reversi-game 等の channel 駆動シナリオを e2e_federation で検証するため、
// gorilla/websocket で /streaming に接続し、Misskey の "channel" envelope
// プロトコル (connect / ch / disconnect) をやり取りする最小限の helper を
// 提供する。WebSocket 経路は production と同じ stream.Dispatcher 配下を
// 経由するので、認証 / channel registry / pubsub すべて実環境と等価。
package e2e_federation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// channelMessage は server → client の channel envelope を unwrap した形。
// `{"type":"channel","body":{"id":"<channel-id>","type":"<event>","body":...}}`
// から body 部分を取り出す。
type channelMessage struct {
	ChannelID string
	EventType string
	Body      json.RawMessage
}

// streamingClient は test 用の最小 WebSocket client。channel 1 つを subscribe
// した状態で送受信を行う。複数 channel を貼りたい場合は別 instance を作る。
type streamingClient struct {
	t         *testing.T
	conn      *websocket.Conn
	channelID string
	mu        sync.Mutex
	queue     []channelMessage
	cond      *sync.Cond
	closed    bool
	closeOnce sync.Once
}

// dialStreaming は token 付きで /streaming に WebSocket 接続を確立する。
// token=nil なら未認証 (一部 channel のみ動く)。
func dialStreaming(t *testing.T, srv *testServer, token *userToken) *websocket.Conn {
	t.Helper()
	url := strings.Replace(srv.BaseURL, "http://", "ws://", 1) + "/streaming"
	if token != nil {
		url += "?i=" + token.Token
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err, "dial %s", url)
	return conn
}

// connectChannel は dialStreaming の上で `connect` envelope を送って channel
// を購読し、receive goroutine を立ち上げて queue にイベントを溜める client
// を返す。t.Cleanup で disconnect + close を保証する。
func connectChannel(t *testing.T, srv *testServer, token *userToken, channelName string, params map[string]any) *streamingClient {
	t.Helper()
	conn := dialStreaming(t, srv, token)

	channelID := fmt.Sprintf("ch-%s-%d", channelName, time.Now().UnixNano())
	c := &streamingClient{t: t, conn: conn, channelID: channelID}
	c.cond = sync.NewCond(&c.mu)

	// connect envelope を送出
	connectMsg := map[string]any{
		"type": "connect",
		"body": map[string]any{
			"id":      channelID,
			"channel": channelName,
			"params":  params,
		},
	}
	require.NoError(t, conn.WriteJSON(connectMsg))

	// receive goroutine
	go c.readLoop()

	t.Cleanup(c.Close)
	return c
}

// readLoop は WebSocket からの "channel" envelope を queue に積む。それ以外
// (connected / api 応答等) は無視する。
func (c *streamingClient) readLoop() {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.closed = true
			c.cond.Broadcast()
			c.mu.Unlock()
			return
		}
		var env struct {
			Type string `json:"type"`
			Body struct {
				ID   string          `json:"id"`
				Type string          `json:"type"`
				Body json.RawMessage `json:"body"`
			} `json:"body"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.Type != "channel" {
			continue
		}
		c.mu.Lock()
		c.queue = append(c.queue, channelMessage{
			ChannelID: env.Body.ID,
			EventType: env.Body.Type,
			Body:      env.Body.Body,
		})
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

// SendChannel は `ch` envelope (channel への in-band message) を送る。
// (Misskey TS の channel.api.send 相当)。
func (c *streamingClient) SendChannel(msgType string, body any) {
	c.t.Helper()
	require.NoError(c.t, c.conn.WriteJSON(map[string]any{
		"type": "ch",
		"body": map[string]any{
			"id":   c.channelID,
			"type": msgType,
			"body": body,
		},
	}))
}

// WaitForEvent は queue から指定 eventType の最初の message を timeout 内に
// 取り出す。見つからなければ test を fail させる。queue 内で順序を保ち、
// 既に消費した message は再度返さない (テスト間で混線しない)。
func (c *streamingClient) WaitForEvent(eventType string, timeout time.Duration) channelMessage {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		for i, m := range c.queue {
			if m.EventType == eventType {
				// 取り出して残りを slide
				c.queue = append(c.queue[:i], c.queue[i+1:]...)
				return m
			}
		}
		if c.closed {
			c.t.Fatalf("connection closed while waiting for event %q (queue=%+v)", eventType, c.queue)
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			c.t.Fatalf("timed out waiting for event %q (queue=%+v)", eventType, c.queue)
		}
		// cond.Wait に timeout を持たせるための小工夫: Goroutine で
		// 一定時間後 Broadcast を投げる。
		notified := make(chan struct{})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), remain)
			defer cancel()
			<-ctx.Done()
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
			close(notified)
		}()
		c.cond.Wait()
		// notified goroutine は cleanup される
		_ = notified
	}
}

// Close は disconnect envelope を送出して WebSocket を閉じる。t.Cleanup で
// 自動呼び出し。複数回呼んでも no-op。
func (c *streamingClient) Close() {
	c.closeOnce.Do(func() {
		_ = c.conn.WriteJSON(map[string]any{
			"type": "disconnect",
			"body": map[string]any{"id": c.channelID},
		})
		_ = c.conn.Close()
	})
}
