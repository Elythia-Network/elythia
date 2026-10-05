// Package imagedecode exposes the server's image decoding limits to plugins.
//
// **プラグインは別 module なので `internal/` を import できない。** 本体は
// #3037 で「宣言寸法の cap → バイト予算 → wasm へ渡す入力の上限 → 同時実行枠」を
// `internal/misc/imagedecode` に集約したが、その入口が公開されていないため、
// プラグインが取得した画像は cap 無しでデコードされていた。取得元が細工した
// 画像を返すと、ヘッダの寸法だけで巨大なラスタを確保できる (Go の大確保失敗は
// `throw("out of memory")` で recover 不能なのでプロセスごと落ちる)。
//
// **`plugin` 本体とは別パッケージにしてある。** ここは画像ライブラリ
// (`kovidgoyal/imaging` / `blezek/tga` 等) に依存するので、`plugin` に置くと
// **`plugin` を import するだけの全プラグインが go.sum にそれらの entry を
// 要求される** (同梱プラグインのビルドが実際に落ちた)。`plugin/peercache` が
// `pgx` を持つのと同じ切り方。
package imagedecode

import (
	"image"

	"github.com/elythia-network/elythia/internal/misc/imagedecode"
)

// MaxImagePixels returns the declared width*height ceiling used for images a
// plugin fetched from a third party. 本体の media proxy と同じ値。
//
// **変数ではなく関数。** 公開変数だとプラグインから代入できてしまうが、
// `DecodeImage` は本体側の値を直接読むので**代入しても何も起きない**
// (上限を緩めたつもりで素通りし、しかも気付けない)。厳しくしたいプラグインは
// `DecodeImageWithPixelCap` を使うこと。
func MaxImagePixels() int64 { return imagedecode.MaxPixels }

// DecodeImage decodes an image with the same limits the server applies to
// remote media.
//
// **取得元からのバイト数を絞るだけでは足りない。** 圧縮された入力が小さくても、
// 展開後のラスタは宣言寸法で決まる。`io.LimitReader` で読み込みを止めていても
// このデコードを通さなければ確保は起きる。
func DecodeImage(data []byte) (image.Image, error) {
	return imagedecode.DecodeWithPixelCap(data, imagedecode.MaxPixels)
}

// DecodeImageWithPixelCap is DecodeImage with an explicit ceiling, for plugins
// that want a tighter limit than the server default.
func DecodeImageWithPixelCap(data []byte, maxPixels int64) (image.Image, error) {
	return imagedecode.DecodeWithPixelCap(data, maxPixels)
}
