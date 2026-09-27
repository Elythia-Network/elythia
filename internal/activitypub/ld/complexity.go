package ld

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrTooComplex is returned when a document would make URDNA2015
// canonicalization take an unbounded amount of CPU time.
var ErrTooComplex = errors.New("jsonld: document too complex to canonicalize")

const (
	// maxBlankNodeLabels bounds how many DISTINCT explicit blank node labels
	// (`_:xxx`) a document may carry.
	//
	// **これが主防御。** URDNA2015 の `hashNDegreeQuads` が全順列を列挙するのは
	// **相互に参照し合う**非一意な blank node に対してで、孤立した同型ノードを
	// いくら並べても爆発しない (実測: 同型ノード 32 個で 1.9ms、伸びない)。
	// JSON はツリーなので、blank node 同士の相互参照は**明示ラベル
	// (`{"@id":"_:b0"}`) でしか作れない**。ラベルの数を絞れば clique の大きさが
	// 頭打ちになる。
	//
	// 実測 (n 個の blank node が互いを指す clique、`@type: "@id"` の述語で接続):
	// n=4 1.6ms / n=5 8.5ms / n=6 73ms / n=7 708ms / n=8 7.8s / n=9 1m52s。
	// **1 段ごとにおよそ 10 倍**。document は n=7 で 956 バイトしかないので、
	// 上限を置かないと 1KB で 1 コアを任意時間焼ける。
	//
	// 4 なら最悪 1.6ms。**Misskey も Mastodon も明示ラベルを出さない**
	// (`third_party/misskey` の renderer に `_:` は 1 件も無い) ので、正当な
	// activity がここに掛かることはまず無い。ノート本文にたまたま `_:x` の
	// 形が現れる可能性は残るが、4 個までは通る。
	maxBlankNodeLabels = 4

	// maxIdenticalBlankNodes bounds how many structurally identical blank
	// nodes a document may contain.
	//
	// 明示ラベルを絞ったうえでの保険。json-gold には作業量を打ち切る機構が
	// 無い (upstream が使う rdf-canonize は `maxWorkFactor = 1` を既定で持つ)
	// ので、見落とした形があっても総量で頭を押さえる。
	maxIdenticalBlankNodes = 8

	// maxBlankNodes bounds the total number of blank nodes.
	//
	// 同じく保険。upstream の受け入れ上限 (添付 16 / 絵文字タグ 128 /
	// プロフィール欄 16) の合計に余裕を持たせた値。
	//
	// **見積もりは下振れしうる。** `@id` / `id` を持つノードは blank node に
	// ならないものとして数えないが、`id` が `@id` へ写像されるかは
	// **document 側の `@context` 次第**。受信の LD-Signature 経路は
	// ld.InboxCompactContext へ compact した文書を normalize するので `id` は
	// AS2 の alias で `@id` になるが、compact を経ない呼び出しでは攻撃者が
	// そこを決められる。だからこの上限を単独の防御とみなさないこと (主防御は
	// maxBlankNodeLabels)。
	maxBlankNodes = 512

	// maxComplexityDepth bounds how deep the estimator walks.
	//
	// JSON 自体に循環は無いので、これは病的に深い入力に対する保険。
	// **超えたら拒否する (fail-closed)。** 打ち切って「見なかったことにする」と、
	// payload を 33 段包むだけで**他の上限がまとめて無効になる** (実測: 同型
	// blank node 1000 個を 40 段包むと素通りした)。
	maxComplexityDepth = 32
)

// CheckComplexity reports whether doc can be canonicalized in bounded time.
//
// 正規化の**前**に呼ぶこと。判定は入力の JSON 構造に対する近似で、
// URDNA2015 の非一意判定そのものではない。
func CheckComplexity(doc any) error {
	c := &complexityCounter{groups: make(map[string]int), labels: make(map[string]struct{})}
	if _, err := c.walk(doc, 0); err != nil {
		return err
	}
	if len(c.labels) > maxBlankNodeLabels {
		return fmt.Errorf("%w: %d explicit blank node labels (limit %d)",
			ErrTooComplex, len(c.labels), maxBlankNodeLabels)
	}
	if c.total > maxBlankNodes {
		return fmt.Errorf("%w: %d blank nodes (limit %d)", ErrTooComplex, c.total, maxBlankNodes)
	}
	for key, n := range c.groups {
		if n > maxIdenticalBlankNodes {
			return fmt.Errorf("%w: %d identical blank nodes (limit %d, shape %s)",
				ErrTooComplex, n, maxIdenticalBlankNodes, key)
		}
	}
	return nil
}

type complexityCounter struct {
	total  int
	groups map[string]int
	// labels は明示的な blank node 識別子 (`_:xxx`) の集合。**位置を問わず
	// 集める** — `@id` の値だけを見る形は、`@context` で `"@type": "@id"` を
	// 宣言した述語の値 (`{"claim": "_:b0"}`) で迂回できる。upstream の
	// context 自身がその宣言を 40 個以上持っている。
	labels map[string]struct{}
}

// walk returns a stable digest of the subtree rooted at v.
//
// `@id` を持つオブジェクトは blank node にならないので数えない。持たない
// ものだけを、部分木の形 (キーと値をすべて含む) でグループ化する。
func (c *complexityCounter) walk(v any, depth int) (string, error) {
	if depth > maxComplexityDepth {
		return "", fmt.Errorf("%w: nesting deeper than %d", ErrTooComplex, maxComplexityDepth)
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		h := sha256.New()
		for _, k := range keys {
			sub, err := c.walk(t[k], depth+1)
			if err != nil {
				return "", err
			}
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write([]byte(sub))
			h.Write([]byte{0})
		}
		digest := hex.EncodeToString(h.Sum(nil)[:8])
		if !hasIRIIdentity(t) {
			c.total++
			c.groups[digest]++
		}
		return digest, nil
	case []any:
		h := sha256.New()
		for _, e := range t {
			sub, err := c.walk(e, depth+1)
			if err != nil {
				return "", err
			}
			h.Write([]byte(sub))
			h.Write([]byte{0})
		}
		return hex.EncodeToString(h.Sum(nil)[:8]), nil
	case string:
		if isBlankNodeLabel(t) {
			c.labels[t] = struct{}{}
			// **ラベルは形のダイジェストから外す。** URDNA2015 は入力ラベルを
			// 捨てて正規化するので、`_:b0` と `_:b1` は同型。残すと clique の
			// 各ノードが別グループに割れて同型カウントが立たない。
			return "s:_:", nil
		}
		return "s:" + t, nil
	case bool:
		return "b:" + strconv.FormatBool(t), nil
	case float64:
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(int64(t*1e6)))
		return "n:" + hex.EncodeToString(buf[:]), nil
	case nil:
		return "z", nil
	default:
		return fmt.Sprintf("o:%T", v), nil
	}
}

// isBlankNodeLabel reports whether s is an explicit JSON-LD blank node
// identifier (`_:` followed by a name).
//
// 文法は N-Triples の BLANK_NODE_LABEL に合わせて緩めに取る (先頭は英数字か
// `_`、以降は英数字 / `_` / `-` / `.`)。**緩い方に倒す** — 拾いすぎても
// 「ラベルが 4 個を超えたら拒否」に掛かるだけで、取りこぼすと迂回される。
func isBlankNodeLabel(s string) bool {
	rest, ok := strings.CutPrefix(s, "_:")
	if !ok || rest == "" {
		return false
	}
	for i, r := range rest {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		case (r == '-' || r == '.') && i > 0:
		default:
			return false
		}
	}
	return true
}

// hasIRIIdentity reports whether the node carries an explicit identifier, in
// which case URDNA2015 labels it with that IRI instead of a blank node.
//
// **`_:` で始まるものは IRI ではない** — あれは blank node 識別子なので、
// 持っていても blank node のまま。identity とみなすと、clique の各ノードが
// まるごとカウントから外れる (この検査が最初に見落とした形)。
func hasIRIIdentity(m map[string]any) bool {
	for _, k := range []string{"@id", "id"} {
		if s, ok := m[k].(string); ok && s != "" && !isBlankNodeLabel(s) {
			return true
		}
	}
	return false
}
