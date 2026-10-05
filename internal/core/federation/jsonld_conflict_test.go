package federation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/federation"
)

// **認可の判断と本処理が同じ document を別々に正規化する。** 同じ canonical へ
// 畳まれるキーが複数あると、Go の map は勝者が走査順で決まるので、ゲートが
// 署名者の actor を見て通し、本処理が詐称 actor で動く組み合わせが成立した。
// `internal/activitypub` 側で拒否しているが、**inbox の入口でも落ちること**を
// ここで固定する (拒否を外すと片方のパッケージだけ緑になる形を避ける)。
func TestProcess_RejectsConflictingJSONLDKeys(t *testing.T) {
	cases := map[string]string{
		"actor と as:actor": `{"id":"https://evil.example/activities/1","type":"Delete",
			"actor":"https://evil.example/users/m","as:actor":"https://victim.example/users/a",
			"object":"https://victim.example/users/a"}`,
		"id と @id": `{"id":"https://evil.example/activities/1","@id":"https://victim.example/activities/1",
			"type":"Delete","actor":"https://evil.example/users/m","object":"https://evil.example/notes/1"}`,
		"type と as:type": `{"id":"https://evil.example/activities/1","type":"Delete","as:type":"Create",
			"actor":"https://evil.example/users/m","object":"https://evil.example/notes/1"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, _, _, _ := newProcessor(t, aliceActor)

			err := p.Process([]byte(body))
			require.Error(t, err, "衝突キーを持つ document を受理している")
			// **retry しない。** 同じ body を投げ直しても結果は変わらないので、
			// 生の error だと inbox job が dead letter に積まれるだけ。
			assert.True(t, errors.Is(err, federation.ErrUnsupportedActivity),
				"衝突キーの document が retry される種類のエラーで落ちている: %v", err)
		})
	}
}

// 同じ値の重複は異常ではないので通す (拒否が広すぎないこと)。
func TestProcess_AcceptsDuplicateKeysWithSameValue(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)

	err := p.Process([]byte(`{"id":"https://remote.example/activities/1","type":"Delete",
		"actor":"https://remote.example/users/alice","as:actor":"https://remote.example/users/alice",
		"object":"https://remote.example/notes/gone"}`))
	// 対象が見つからない等で error にはなりうるが、衝突として弾かれてはいけない。
	if err != nil {
		assert.NotContains(t, err.Error(), "conflicting json-ld keys",
			"同じ値の重複を衝突として弾いている")
	}
}
