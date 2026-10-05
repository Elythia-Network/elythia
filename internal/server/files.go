package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// filesDriveLookup is the minimal interface filesHandler needs to decide
// whether a request maps to a locally-stored drive_file row. Implemented
// by repository.DriveFileRepository; abstracted here so handler tests can
// stub without standing up a real DB.
type filesDriveLookup interface {
	FindByAnyAccessKey(accessKey string) (*model.DriveFile, error)
}

// driveAccessKeyMaxRunes は `drive_file.accessKey` の列幅。
const driveAccessKeyMaxRunes = 256

// storableAccessKey reports whether the URL path parameter could name a stored
// object at all.
//
// 見るのは 3 つだけ: 列に入る値か (NUL / 不正な UTF-8 / 幅)、パス要素として
// 危なくないか (`/` `\` `.` `..`)、空でないか。**形式そのものは検査しない** —
// access key の綴りは `newAccessKey` の実装詳細で、過去に保存された行の綴りまで
// 縛ると既存ファイルが引けなくなる。
func storableAccessKey(key string) bool {
	if key == "" || !colfit.Fits(key, driveAccessKeyMaxRunes) {
		return false
	}
	if strings.ContainsAny(key, `/\`) {
		return false
	}
	return key != "." && key != ".."
}

// filesHandler serves `GET /files/:accessKey`.
//
// `useObjectStorage=true` な instance では primary が S3Storage に切り替わるが、
// Misskey TS から S3 移行する前にアップロードされた drive_file 行は
// `storedInternal=true` のままローカル FS に残っており、TS upstream の
// FileServerService も storedInternal を見て storage を切り替える。同じ挙動に
// 合わせるため、DB を引いて storedInternal=true なら local を、false なら
// primary を使う (#1414)。
//
// **行が無ければ 404、lookup が落ちたら 500 (#3037)。** 以前は「行が無い /
// DB error / lookup 未配線」をまとめて primary に倒していたが、それだと
// `drive_file` 行を確認せず実体を配ることになる (削除したのに実体が残って
// いると、URL を知っている人には削除が効かない)。lookup == nil のときだけは
// 従来どおり primary に倒す (配線していない構成の最低保証)。
//
// **#2315 の「primary がローカルなら DB を引かない」は撤回した。**
// storedInternal の判定だけなら確かに無意味だが、**行があるかどうかの判定**は
// 無意味ではない。
//
// **ストレージ側の失敗は種別で分ける。** `ErrObjectNotFound` だけが 404 で、
// それ以外 (S3 の認証失効 / throttling / 5xx / FS の I/O エラー) は 500。
//
// MIME type はファイル内容の先頭から自動判定し、`http.ServeContent` で
// Range / If-Modified-Since / Content-Length 対応の正しい応答を返す。
//
// 旧実装は echo の `c.Stream` + `io.MultiReader` で chunked transfer に
// 倒していたが、Cloudflare Tunnel 経由のデプロイで bigger-than-buffer
// (33KB 程度) なファイルが truncate される問題があった (#730)。明示
// Content-Length + Cache-Control: no-transform でこれを回避する。
func filesHandler(lookup filesDriveLookup, primary, local coredrive.Storage) echo.HandlerFunc {
	return func(c echo.Context) error {
		key := c.Param("accessKey")
		// **オブジェクトを名指しできないキーは、引く前に 404 (#3037)。**
		//
		// `accessKey` は varchar(256) の列の値なので、そこに入らない値は
		// **どの行とも一致しえない** = 「無い」が事実 (#3025 と同じ判断)。
		// これを storage へ渡すと、`LocalStorage.Get` が `os.IsNotExist` 以外
		// (`ENAMETOOLONG` / `EINVAL`) を素の `*PathError` で返し、下の分岐が
		// **500 + accessKey を丸ごと載せた Error ログ**にする。この route は
		// `api` グループの外で**認証もレートリミットも無い**ので、
		// `GET /files/a%00b` を叩くだけで 5xx レートとログを誰でも汚せる
		// (#3025 が塞いだ形の再導入だった)。
		//
		// **パス要素も落とす。** `/files/.` は `os.Open` がディレクトリを
		// 開けてしまい、200 + 空ボディを返していた。
		if !storableAccessKey(key) {
			return c.NoContent(http.StatusNotFound)
		}
		storage := primary
		// **行が無いものは配らない (#3037)。**
		//
		// 以前は primary が非ローカルのときだけ DB を引き、それ以外は
		// **`drive_file` 行を一度も確認せずストレージから直接返して**いた。
		// upstream の `FileServerService` は毎回
		// `resolveFileByAccessKey` を通し、行が無ければ 404 にする。
		//
		// 差が出るのは**行を消したのに実体が残っている**とき — 削除時の
		// オブジェクト削除が失敗した (S3 の一時障害など) 場合や、同じ
		// バケット / ディレクトリを他の用途と共有している場合。access key は
		// 推測できないので総当たりはできないが、**一度公開した URL を知って
		// いる人には削除が効かない**。「消したのにまだ見られる」はドライブの
		// 削除機能そのものの前提を崩す。
		//
		// **ローカルストレージ構成でも 1 回引くようになる。** 非ローカル構成
		// では元から毎回引いていたので増えない。
		if lookup != nil {
			f, err := lookup.FindByAnyAccessKey(key)
			switch {
			case err == nil && f != nil:
				// storedInternal な行は object storage 移行前のもので、
				// 実体はローカル FS に残っている (#1414)。
				if f.StoredInternal && local != nil {
					storage = local
				}
			case repository.IsNotFound(err) || (err == nil && f == nil):
				return c.NoContent(http.StatusNotFound)
			default:
				// **DB 障害を 404 に潰さない (#2792)。** 「行が無い」と
				// 「引けなかった」は別で、後者を 404 にすると監視に 5xx が
				// 立たないまま全ファイルが消えたように見える。
				slog.Error("server: drive file lookup failed", "accessKey", key, "err", err)
				return c.NoContent(http.StatusInternalServerError)
			}
		}
		body, err := storage.Get(key)
		if err != nil {
			// **ストレージ障害を「無い」に潰さない (#2792)。** #2990 が
			// `S3Storage.Get` で種別を分けたのはこのためで、他の呼び出し側は
			// すべて直っているのにここだけ残っていた。S3 の認証失効 /
			// throttling / 5xx が全部 404 になると、クライアントからは区別が
			// 付かず**監視にも 5xx が立たない**。
			if errors.Is(err, coredrive.ErrObjectNotFound) {
				return c.NoContent(http.StatusNotFound)
			}
			slog.Error("server: drive storage read failed", "accessKey", key, "err", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		defer body.Close()

		// LocalStorage は *os.File を返すので io.ReadSeeker として扱える。
		// 互換性のため type assertion で seekable かを確認し、非対応 (将来
		// S3 等の non-seekable storage を増やした場合) なら全 body を memory
		// に読み込む fallback にフォールバックする。
		// modtime も *os.File なら Stat() から取得して If-Modified-Since の
		// 304 経路を有効にする。それ以外は零値で http.ServeContent が
		// Last-Modified を出さない (Cache-Control: immutable で代替)。
		var modtime time.Time
		seeker, ok := body.(io.ReadSeeker)
		if ok {
			if f, isFile := body.(*os.File); isFile {
				if st, err := f.Stat(); err == nil {
					modtime = st.ModTime()
				}
			}
		} else {
			data, rerr := io.ReadAll(body)
			if rerr != nil {
				return c.NoContent(http.StatusInternalServerError)
			}
			seeker = bytes.NewReader(data)
		}

		buf := make([]byte, coredrive.MIMESniffLen)
		n, _ := seeker.Read(buf)
		contentType := coredrive.DetectMIME(buf[:n])
		// #2106 H3: sniff した MIME を browser-safe allowlist に通し、非該当
		// (text/html, image/svg+xml, text/xml 等) は application/octet-stream に
		// 矯正する。これを欠くと任意アップロードが file origin から active content
		// として実行され stored XSS になる (upstream FileServerUtils.getSafeContentType)。
		contentType = coredrive.BrowserSafeContentType(contentType)
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return c.NoContent(http.StatusInternalServerError)
		}

		// `Cache-Control: no-transform` で Cloudflare Polish 等 CDN 中間層
		// による画像再エンコードを抑止する (#730)。Polish は animated WebP
		// を別 size で出力するため UDS デプロイ環境で「絵文字がチカチカ
		// する + サイズが切れる」現象を起こす。`immutable` で長期 cache 化、
		// `no-transform` で binary 1:1 配信を契約する。
		c.Response().Header().Set("Cache-Control", "max-age=31536000, immutable, no-transform")
		// #2106 H3: file origin からの script/object 実行を封じる。CSP は upstream
		// FileServerService と同じ値。`Content-Disposition: inline` も upstream 相当
		// (ただし upstream は `contentDisposition('inline', filename)` で filename も
		// 載せる)。**この 3 つのうち mk-go 独自なのは nosniff だけ** — upstream の
		// backend が明示的に付ける箇所は無い (直上の `Cache-Control` の
		// `no-transform` も独自だが、そちらは 1 つ上のコメントで説明済み)。#2782 で global にも付くようになったが、
		// ここは file 配信固有の多層防御として残す。
		c.Response().Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
		c.Response().Header().Set("X-Content-Type-Options", "nosniff")
		c.Response().Header().Set("Content-Disposition", "inline")
		c.Response().Header().Set(echo.HeaderContentType, contentType)
		http.ServeContent(c.Response(), c.Request(), key, modtime, seeker)
		return nil
	}
}
