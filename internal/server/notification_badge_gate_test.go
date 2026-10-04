package server

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// 通知のバッジ (`.subIcon`) に padding を書かせない。
//
// **`.subIcon` は `box-sizing: border-box` + `line-height: 20px` で中身を中央に
// 置く。** padding を足すと内容領域だけ縮み、アイコンが下へずれる。CSS としては
// 完全に正当なので、型検査も lint も何も言わない。**目で見るまで気付けない。**
//
// #2868 が実際にこれを書いて本番で指摘され外した。#2987 でまた書いて、また
// 本番で指摘された (「バッジがずれてる気がする」)。**2 回踏んだので機械で止める。**
//
// 判定は「`.t_*` の宣言ブロックに padding が無いこと」。他の要素
// (`.abuseReportCommands` 等) は対象外で、バッジの色分けクラスだけを見る。
func TestNotificationBadgeClassesHaveNoPadding(t *testing.T) {
	path := filepath.Join(repoRootDir(t), "frontend",
		"packages", "frontend", "src", "components", "MkNotification.vue")
	src, err := os.ReadFile(path)
	if err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", path)
	}

	// `.t_xxx {` から次の `}` までを 1 ブロックとして取る。
	blocks := regexp.MustCompile(`(?s)\n\.(t_[A-Za-z0-9_]+) \{(.*?)\n\}`).FindAllStringSubmatch(string(src), -1)
	require.NotEmpty(t, blocks,
		"%s から .t_* の宣言を 1 つも読めなかった (書式が変わった?)", path)
	// **数も確かめる。** 正規表現が一部しか拾わなくなると、検査が静かに縮む。
	require.GreaterOrEqual(t, len(blocks), 15,
		"%s の .t_* が %d 件しか見つからない。正規表現が空振りしていないか",
		path, len(blocks))

	for _, b := range blocks {
		require.NotContainsf(t, b[2], "padding",
			".t_%s に padding がある。`.subIcon` は box-sizing: border-box で\n"+
				"中身を中央に置くので、padding を足すと内容領域だけ縮んで\n"+
				"アイコンが下へずれる (#2868 / #2987 で 2 回踏んだ)。\n"+
				"大きさを変えたいなら font-size で調整すること。", b[1])
	}

	// 前提が変わったら落とす — `.subIcon` が border-box を止めたなら、この
	// gate の理由自体が無くなるので書き直すこと。
	require.Contains(t, string(src), "box-sizing: border-box",
		"`.subIcon` が box-sizing: border-box を持たない。この gate の前提が変わっている")
}
