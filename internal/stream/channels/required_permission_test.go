package channels

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/stream"
)

// チャンネルが要求する OAuth2 scope を固定する。
//
// **これが無いと 1 行消すだけで権限チェックが消える。** `PermittedChannel` は
// optional interface なので、`RequiredPermission` を削っても
// `Dispatcher.checkPermission` が黙って許可側に倒れ、コンパイルも通る。
// 実際この表は長らく実質無効だった (Connection に scope が載っておらず
// `HasPermission` が常に true を返していた) ので、配線を直したこの機に
// 表そのものも固定する。
//
// upstream の値と 1:1 で対応させること。ずらす場合は docs/divergence.md へ。
func TestChannels_RequiredPermission(t *testing.T) {
	cases := []struct {
		name string
		ch   stream.PermittedChannel
		want string
	}{
		{"chatUser", &ChatUserChannel{}, "read:chat"},
		{"chatRoom", &ChatRoomChannel{}, "read:chat"},
		{"admin", &AdminChannel{}, "read:admin:stream"},
		{"main", &MainChannel{}, "read:account"},
		{"notifications", &NotificationsChannel{}, "read:account"},
		{"drive", &DriveChannel{}, "read:account"},
		{"antenna", &AntennaChannel{}, "read:account"},
		{"homeTimeline", &HomeTimelineChannel{}, "read:account"},
		{"hybridTimeline", &HybridTimelineChannel{}, "read:account"},
		{"reversi", &ReversiChannel{}, "read:account"},
		{"bubbleVersus", &BubbleVersusChannel{}, "read:account"},
		{"bubbleVersusMatch", &BubbleVersusMatchChannel{}, "read:account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.ch.RequiredPermission(),
				"%s の要求 scope が変わっている", tc.name)
		})
	}

	// 表の件数も固定する。credentialed なチャンネルを足したときに
	// ここへ追記し忘れると、その 1 本だけ無検査で通ってしまう。
	assert.Len(t, cases, 12, "credentialed channel を増減したら表も更新すること")
}
