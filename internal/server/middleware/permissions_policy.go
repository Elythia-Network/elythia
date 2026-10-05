package middleware

import "github.com/labstack/echo/v4"

// permissionsPolicyValue disables browser features the UI never uses.
//
// **落とすものだけを列挙する。** 未指定の機能は既定 (`self`) のままなので、
// ここに書かないことが「許可」になる。使っている機能を誤って落とすと、原因の
// 分かりにくい形で壊れる。
//
// **ソースの grep だけで判断しない。** frontend は npm 依存経由でブラウザ機能を
// 使う。実際 `camera` は当初落とす予定だったが、`/qr` の読み取りタブが
// `qr-scanner` パッケージ経由で `getUserMedia` を呼ぶ (`pages/qr.read.vue`)。
// 落とすと許可を求める前に `NotAllowedError` になり、タブを開くたびにエラー
// ダイアログが出て機能が死ぬ。**判断する前に `node_modules` と
// `frontend/built/` も見ること。**
//
// 実際に確認したうえで選んだ (ソース・`node_modules`・ビルド成果物を grep):
//
//   - `microphone`: 音声を取る経路が無い。`qr-scanner` も `audio:false` で呼ぶ
//   - `geolocation`: note に位置情報を載せる機能が無い。バンドルにも 0 件
//   - `payment`: `new PaymentRequest` / `window.PaymentRequest` はバンドルに
//     0 件 (`property-information` の HTML 属性表に `allowPaymentRequest` が
//     載っているだけ)
//
// **落とさないもの:**
//
//   - `camera`: 上記のとおり `/qr` が使う
//   - `fullscreen`: `MkUrlPreview` / `MkYouTubePlayer` の iframe が `allow` に
//     載せ、`useNativeUiForVideoAudioPlayer` ではネイティブ `<video controls>` の
//     全画面ボタンが使う
//   - `display-capture`: Sentry のフィードバック用スクリーンショットが
//     `getDisplayMedia` を呼ぶ (ビルド成果物で確認)
//
// プラグインが frontend に .vue を注入できる (docs/plugins/) ので、将来ここに
// 挙げた機能を使うプラグインが出たら値を緩める。
const permissionsPolicyValue = "microphone=(), geolocation=(), payment=()"

// PermissionsPolicy returns a GLOBAL middleware that sets `Permissions-Policy`.
//
// **upstream には無い mk-go 独自の hardening** (#2782)。upstream の backend に
// `Permissions-Policy` を付ける箇所は無い。XSS が別途成立したときに、攻撃者が
// カメラやマイクへ手を伸ばす余地を先に潰しておく。
func PermissionsPolicy() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set("Permissions-Policy", permissionsPolicyValue)
			return next(c)
		}
	}
}
