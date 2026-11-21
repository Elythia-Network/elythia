package middleware

import (
	"strings"

	"github.com/labstack/echo/v5"
	emiddleware "github.com/labstack/echo/v5/middleware"
)

// BodyLimitByPath returns a GLOBAL middleware that caps the request body size by
// path, matching upstream's per-route bodyLimit:
//
//   - /api/*  → 1MiB (upstream ApiServerService の bodyLimit: 1024*1024)
//   - /inbox, /users/:id/inbox → 64KiB (upstream ActivityPubServerService の bodyLimit: 1024*64)
//   - /api/drive/files/create → maxFileSize + multipart framing の余白
//   - /api/drive/files/create-chunked/append → 33MiB
//   - /api/plugin/<name>/_peer → pluginPeerLimits が持つプラグインごとの値
//   - その他 → 1MiB (upstream の fastify 既定と同値)
//
// これは **auth.Authenticate より前** (global pre-auth) に登録する必要がある。
// auth.Authenticate は token 抽出のため body を io.ReadAll する (extractToken) が、
// echo の middleware 順は global → group → route なので、route/group に BodyLimit を
// 置いても auth の read が先に走って無制限に body を消費し bypass される (#1958 / #2075)。
// 本 middleware を auth より前に置くことで auth・JSONBodyParse・handler の全 read が
// limitedReader 経由になり cap される。
//
// 超過時は upstream fastify と同じ 413 (echo BodyLimit が ContentLength 即時 reject /
// chunked は limitedReader が 413 error を返す。後者は JSONBodyParse / inbox handler が
// その 413 のエラーを伝播する。apierr.HTTPStatusError で見分ける)。
//
// upload endpoint (drive/files/create) は 1MiB ではなく maxFileSize ベースの上限に
// する。content-type が multipart というだけで /api 全体を除外すると、非 upload
// endpoint (例: 未認証到達可能な /api/meta) に multipart を投げて 1MiB を bypass し
// ParseMultipartForm に disk spill させる DoS 穴になるので、path で絞る。
//
// **無制限 (`next(c)`) にしてはいけない。** かつて「RequireAuth + write:drive で
// 保護されているので file サイズは drive handler の maxFileSize が bound する」を
// 根拠に無制限にしていたが、これは成立しない。route の RequireAuth より前に
// global な auth.Authenticate が動き、multipart のとき token を探して
// `c.FormValue("i")` を呼ぶ。Go の ParseMultipartForm はメモリ 32MiB を超えた分を
// **上限なくディスクへ書き出す**ので、未認証のまま巨大な multipart を並列に
// 送り付けるだけでディスクと file descriptor を食い潰せる。つまり「auth が守る」
// と言いながら、その auth 自身がパーサの引き金になっていた。
//
// upstream も同じ形で守っている。requireFile endpoint に raw body の bodyLimit は
// 無いが、@fastify/multipart を `limits: { fileSize: config.maxFileSize, files: 1 }`
// で登録しており、パーサ自身が maxFileSize で打ち切る (ApiServerService.ts)。
// mk-go 側は body 全体に掛けるので、framing の余白を足した値にする。
// pluginPeerLimits はプラグイン同士の通信 (#2537) の受け口に掛ける、パスごとの
// 上限。**ここに置くしかない。** 受け口は署名を検証する前に body を読み終えて
// いる (global な auth.Authenticate が token 抽出で読む) ので、handler 側で
// 判定しても消費は止まらない。nil / 空なら /api の 1MiB に倒れる。
func BodyLimitByPath(maxFileSize int64, pluginPeerLimits map[string]int64) echo.MiddlewareFunc {
	apiBL := emiddleware.BodyLimit(1 << 20)    // = 1024*1024 = 1048576
	inboxBL := emiddleware.BodyLimit(64 << 10) // = 1024*64   = 65536
	// **既知のパス以外も上限を持たせる。**
	//
	// かつてここは `next(c)` で無制限だった。上の doc が /api について
	// 書いている穴 (auth.Authenticate が token 抽出で body を読むので、
	// route の認証より前に消費が起きる) は **パスを問わず成立する** —
	// echo は未登録のパスでも global middleware を通すので、`POST /` でも
	// 存在しないパスでも `io.ReadAll` が全量をヒープに載せる。404/405 を
	// 返す前の出来事なので、ルートの存在すら要らない。
	//
	// upstream は `Fastify({...})` で bodyLimit を上書きしていないので、
	// fastify 既定の 1MiB が **404 ルートを含む全ルート**に効く。加えて
	// upstream は body を読む global hook を持たない (token を読むのは
	// /api のルート内だけ)。ここを同値にすることで揃う。
	defaultBL := emiddleware.BodyLimit(1 << 20)
	// upstream の limits.fileSize は **ファイル本体**に掛かるが、こちらは body
	// 全体に掛かる。boundary / part header / 同送される他フィールド (i など) の
	// 分だけ余白を足さないと、maxFileSize ちょうどのファイルが弾かれる。
	uploadBL := emiddleware.BodyLimit(uploadBodyLimit(maxFileSize))
	// chunked upload の append (#2313)。1 リクエスト = 1 チャンクなので、
	// create のように無制限にはせず固定上限を掛ける。値は
	// drive.MaxChunkSizeMb (32MiB) + multipart framing の余白。実際に受理する
	// サイズはセッションに固定された chunkSize ちょうどであることを handler が
	// 別途強制するので、ここは「それ以上は読み込ませない」ための粗い上限。
	chunkBL := emiddleware.BodyLimit(33 << 20)
	const (
		driveUploadPath = "/api/drive/files/create"
		chunkAppendPath = "/api/drive/files/create-chunked/append"
	)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		// limitedReader pool は per-route で 1 度だけ構築する (BodyLimit の
		// MiddlewareFunc を next で 1 回 wrap)。本 middleware は global なので
		// next は router dispatch、pool 構築も 1 度きり。
		apiNext := apiBL(next)
		inboxNext := inboxBL(next)
		defaultNext := defaultBL(next)
		chunkNext := chunkBL(next)
		uploadNext := uploadBL(next)
		peerNext := make(map[string]echo.HandlerFunc, len(pluginPeerLimits))
		for path, limit := range pluginPeerLimits {
			peerNext[path] = emiddleware.BodyLimit(limit)(next)
		}
		return func(c *echo.Context) error {
			p := c.Request().URL.Path
			// **/api の分岐より先に見る。** 後ろに置くと 1MiB 側に吸われる。
			if h, ok := peerNext[p]; ok {
				return h(c)
			}
			switch {
			case p == driveUploadPath:
				// 唯一の multipart upload endpoint。auth より前に上限を掛ける
				// (関数 doc 参照)。
				return uploadNext(c)
			case p == chunkAppendPath:
				// create と同じく RequireAuth + RequireNotMoved +
				// write:drive で保護された route。未認証で到達できる経路は
				// 増えていない。
				return chunkNext(c)
			case p == "/api" || strings.HasPrefix(p, "/api/"):
				return apiNext(c)
			case isInboxPath(p):
				return inboxNext(c)
			}
			return defaultNext(c)
		}
	}
}

// multipartFramingMargin is the slack added on top of maxFileSize so a file of
// exactly the allowed size still fits once multipart framing is included.
//
// boundary 行・part ごとの header・同送される他フィールド (i / folderId /
// name など) を足しても 1MiB には遠く及ばない。broad にとってあるのは、ここを
// 切り詰めても攻撃者の得られる余地 (1MiB) が誤差でしかない一方、足りないと
// 正当なアップロードが 413 で落ちるため。
const multipartFramingMargin int64 = 1 << 20

// defaultUploadBodyLimit mirrors config.defaultMaxFileSize (250MB) and is used
// when the caller passes a non-positive size.
//
// 設定漏れや将来の wiring ミスで 0 が渡ったときに「上限なし」へ倒れると、
// 直したはずの穴がそのまま戻る。既定値に落として必ず上限が掛かるようにする。
const defaultUploadBodyLimit int64 = 262144000

// uploadBodyLimit returns the byte cap applied to the multipart upload route.
func uploadBodyLimit(maxFileSize int64) int64 {
	if maxFileSize <= 0 {
		maxFileSize = defaultUploadBodyLimit
	}
	return maxFileSize + multipartFramingMargin
}

// isInboxPath reports whether p is an ActivityPub inbox route (/inbox or
// /users/:id/inbox)。
func isInboxPath(p string) bool {
	return p == "/inbox" || (strings.HasPrefix(p, "/users/") && strings.HasSuffix(p, "/inbox"))
}
