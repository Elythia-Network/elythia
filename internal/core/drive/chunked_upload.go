package drive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/safemath"
	"gorm.io/datatypes"
)

// Chunked upload limits (#2313).
const (
	// MinChunkSizeMb is the S3 minimum part size for every part but the last.
	// Going below it makes CompleteMultipartUpload fail.
	MinChunkSizeMb = 5
	// MaxChunkSizeMb keeps one append well under typical reverse-proxy body
	// limits (Cloudflare caps bodies at 100MB on Free/Pro) and bounds the
	// per-request memory a single chunk can pin.
	MaxChunkSizeMb = 32
	// DefaultChunkSizeMb is what an unconfigured instance uses. 10 MiB is twice
	// the S3 minimum and a tenth of the Cloudflare body cap; more importantly it
	// stays inside Cloudflare's 100 second timeout on slow uplinks (10 MiB takes
	// ~17s at 5 Mbps, 100 MB would take ~160s and time out).
	DefaultChunkSizeMb = 10

	// MaxMultipartParts is the S3 limit on parts per multipart upload.
	MaxMultipartParts = 10000

	// Session TTL bounds. 短すぎると遅い回線で転送中に失効し、長すぎると
	// 未完了マルチパートアップロードの課金が伸びる。
	MinSessionTTLMinutes     = 5
	MaxSessionTTLMinutes     = 1440
	DefaultSessionTTLMinutes = 60

	// finishGracePeriod is how long the GC leaves an expired session alone while
	// a finish is still claimed on it. finish は期限内に始まれば期限後も走り
	// 続けるので、猶予なしだと正常な完了を GC が横から壊しうる。
	finishGracePeriod = 10 * time.Minute
)

// Chunked upload errors (#2313).
var (
	// ErrChunkedUploadUnavailable means the instance cannot serve chunked
	// uploads at all: disabled in meta, or the storage backend has no multipart
	// support (local storage).
	ErrChunkedUploadUnavailable = errors.New("chunked upload unavailable")
	// ErrChunkedUploadNotAllowed means the caller's role policy forbids it.
	ErrChunkedUploadNotAllowed = errors.New("chunked upload not allowed")
	// ErrTooManyUploadSessions means the caller already holds the maximum
	// number of concurrent sessions.
	ErrTooManyUploadSessions = errors.New("too many upload sessions")
	// ErrPendingUploadLimitExceeded means the caller's outstanding declared
	// bytes would exceed their allowance.
	ErrPendingUploadLimitExceeded = errors.New("pending upload limit exceeded")
	// ErrUploadSessionNotFound covers missing, expired and others' sessions
	// alike so the response is not an existence oracle.
	ErrUploadSessionNotFound = errors.New("upload session not found")
	// ErrUploadSessionBusy means another request is mutating the session.
	ErrUploadSessionBusy = errors.New("upload session busy")
	// ErrInvalidChunkSize means the chunk was empty, overshot the declared total
	// or was not exactly chunkSize while not being the final chunk.
	ErrInvalidChunkSize = errors.New("invalid chunk size")
	// ErrChunkContentMismatch means an already-recorded index was re-sent with
	// different bytes.
	ErrChunkContentMismatch = errors.New("chunk content mismatch")
	// ErrIncompleteUpload means finish was called before all bytes arrived, or
	// the recorded parts are not a contiguous run.
	ErrIncompleteUpload = errors.New("incomplete upload")
	// ErrInvalidUploadSize means the declared size was non-positive or needs
	// more parts than the backend allows.
	ErrInvalidUploadSize = errors.New("invalid upload size")
)

// ChunkIndexError reports which chunk index the session expects next.
//
// クライアントは append の応答が失われたのか、送信自体が失敗したのかを
// 区別できない。期待 index を返すことで、どちらの向きにリクエストを取り
// こぼしても再同期して続きから送れるようにする。
type ChunkIndexError struct {
	Expected int
}

func (e *ChunkIndexError) Error() string {
	return fmt.Sprintf("unexpected chunk index (expected %d)", e.Expected)
}

// ChunkedUploadSettings is the instance-level configuration resolved from the
// `meta` table, already clamped to the supported range.
type ChunkedUploadSettings struct {
	Enabled                bool
	ChunkSize              int64
	SessionTTL             time.Duration
	MaxSessionsPerUser     int
	MaxPendingBytesPerUser int64
}

// ChunkedUploadSettingsFromMeta reads and clamps the chunked upload settings.
//
// clamp するのは、admin UI 以外の経路 (直接 SQL / 古い行) で範囲外の値が入って
// いても機能が壊れないようにするため。特に chunkSize が S3 の最小パートサイズを
// 下回ると CompleteMultipartUpload が必ず失敗する。
func ChunkedUploadSettingsFromMeta(m *model.Meta) ChunkedUploadSettings {
	if m == nil {
		return ChunkedUploadSettings{}
	}
	return ChunkedUploadSettings{
		Enabled:                m.ChunkedUploadEnabled,
		ChunkSize:              int64(clampInt(m.ChunkedUploadChunkSizeMb, MinChunkSizeMb, MaxChunkSizeMb, DefaultChunkSizeMb)) * 1024 * 1024,
		SessionTTL:             time.Duration(clampInt(m.ChunkedUploadSessionTTLMinutes, MinSessionTTLMinutes, MaxSessionTTLMinutes, DefaultSessionTTLMinutes)) * time.Minute,
		MaxSessionsPerUser:     m.ChunkedUploadMaxSessionsPerUser,
		MaxPendingBytesPerUser: int64(m.ChunkedUploadMaxPendingMbPerUser) * 1024 * 1024,
	}
}

// clampInt bounds v to [lo, hi], falling back to def when v is unset (<= 0).
func clampInt(v, lo, hi, def int) int {
	if v <= 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// SetChunkedUpload wires chunked upload support (#2313). Both arguments must be
// non-nil for the feature to be offered; settings is consulted per request so
// admin changes take effect without a restart.
func (s *Service) SetChunkedUpload(repo repository.ChunkedUploadSessionRepository, settings func() ChunkedUploadSettings) {
	s.chunkedRepo = repo
	s.chunkedSettings = settings
}

// ChunkedUploadCapability reports the effective settings, or ok=false when
// chunked upload is not available on this instance.
//
// ローカルストレージ構成では MultipartStorage を満たさないので ok=false になり、
// /api/meta の能力告知も出ない。フロントエンドはそれを見て従来の単発アップロード
// に倒れる。
//
// backend は meta から都度解決される (#2315) ため、admin がオブジェクトストレージを
// 有効にすればここも再起動なしで ok=true に変わる。逆に進行中セッションの最中に
// 設定を変えると、そのセッションの uploadId は旧 backend のものなので append /
// finish が失敗する。破棄されたセッションは期限切れ GC が回収する。
func (s *Service) ChunkedUploadCapability() (ChunkedUploadSettings, bool) {
	if s.chunkedRepo == nil || s.chunkedSettings == nil {
		return ChunkedUploadSettings{}, false
	}
	if _, ok := ResolveStorage(s.storage).(MultipartStorage); !ok {
		return ChunkedUploadSettings{}, false
	}
	settings := s.chunkedSettings()
	if !settings.Enabled || settings.ChunkSize <= 0 {
		return ChunkedUploadSettings{}, false
	}
	return settings, true
}

// chunkedMultipart returns the settings and the multipart-capable storage, or
// ErrChunkedUploadUnavailable.
func (s *Service) chunkedMultipart() (ChunkedUploadSettings, MultipartStorage, error) {
	settings, ok := s.ChunkedUploadCapability()
	if !ok {
		return ChunkedUploadSettings{}, nil, ErrChunkedUploadUnavailable
	}
	ms, ok := ResolveStorage(s.storage).(MultipartStorage)
	if !ok {
		return ChunkedUploadSettings{}, nil, ErrChunkedUploadUnavailable
	}
	return settings, ms, nil
}

// StartChunkedUploadInput is the parameter set for Service.StartChunkedUpload.
type StartChunkedUploadInput struct {
	User *model.User
	Name string
	// Size is the client's declaration. It is never trusted as the real size;
	// it only bounds what append will accept.
	Size           int64
	Comment        *string
	FolderID       *string
	IsSensitive    bool
	Force          bool
	RequestIP      *string
	RequestHeaders datatypes.JSON
}

// StartChunkedUpload opens an upload session after checking every quota the
// finished file would be subject to. It does not touch object storage: the
// content type must be sniffed from real bytes, which only arrive with the
// first append, and CreateMultipartUpload needs the content type up front.
func (s *Service) StartChunkedUpload(_ context.Context, in StartChunkedUploadInput) (*model.ChunkedUploadSession, error) {
	settings, _, err := s.chunkedMultipart()
	if err != nil {
		return nil, err
	}
	if in.User == nil {
		return nil, ErrChunkedUploadNotAllowed
	}
	if in.Size <= 0 {
		return nil, ErrInvalidUploadSize
	}
	// パート数上限。ここを超えると CompleteMultipartUpload が通らないので、
	// 受け取り始める前に弾く。
	parts := in.Size / settings.ChunkSize
	if in.Size%settings.ChunkSize != 0 {
		parts++
	}
	if parts > MaxMultipartParts {
		return nil, ErrInvalidUploadSize
	}

	// policy が引けない構成 (roleChecker 未配線 / 取得失敗) は fail-closed。
	// 分割アップロードは状態を持つぶん濫用の余地が大きいので、gate が効かない
	// 状態で素通しにはしない。
	if s.roleChecker == nil {
		return nil, ErrChunkedUploadNotAllowed
	}
	policies := s.roleChecker.GetUserPolicies(in.User.ID)
	if policies == nil {
		return nil, ErrChunkedUploadNotAllowed
	}
	if allowed, ok := policies[role.PolicyCanUseChunkedUpload].(bool); !ok || !allowed {
		return nil, ErrChunkedUploadNotAllowed
	}

	now := time.Now()

	// maxFileSizeMb / driveCapacityMb は Upload と同じ gate を start でも通す。
	// Upload は remote user を skip するが (expireOldFile 相当が無いため)、
	// こちらは remote / local を問わず適用する: 分割アップロードは申告値を
	// 信用して受け入れ枠を確保する経路なので、gate が外れる分岐を作らない。
	//
	// **値の読み取りはpolicyMegabytesに通す。** このhelperはpolicyNumberを介して
	// int/float64を正規化してからbyte数へ飽和変換する。素の `.(int)` だと
	// float64で型アサーションに失敗し、上限違反で弾くのではなく**上限そのものが
	// 消える**。Upload側も同じhelperを通すため、同じpolicyが経路によって効いたり
	// 効かなかったりしない (#2611)。
	if maxBytes, ok := policyMegabytes(policies["maxFileSizeMb"]); ok {
		if in.Size > maxBytes {
			return nil, ErrMaxFileSizeExceeded
		}
	}

	pending, err := s.chunkedRepo.PendingBytesByUser(in.User.ID, now)
	if err != nil {
		return nil, fmt.Errorf("count pending chunked uploads: %w", err)
	}
	if capacityBytes, ok := policyMegabytes(policies["driveCapacityMb"]); ok {
		usage, err := s.fileRepo.UsageByUser(in.User.ID)
		if err != nil {
			// Upload と同じ理由で握り潰さない。usage=0 として素通しにすると
			// transient DB error のあいだ容量制限が事実上無効になる。
			return nil, fmt.Errorf("calc drive usage: %w", err)
		}
		// 未完了セッションの申告分も加算する。これが無いと「残容量ぎりぎりの
		// セッションを複数開く」で driveCapacityMb を丸ごと迂回できる。
		if safemath.SumExceedsInt64(capacityBytes, usage, pending, in.Size) {
			return nil, ErrNoFreeSpace
		}
	}

	if maxPending, ok := policyMegabytes(policies[role.PolicyChunkedUploadMaxPendingMb]); ok {
		if settings.MaxPendingBytesPerUser > 0 && maxPending > settings.MaxPendingBytesPerUser {
			maxPending = settings.MaxPendingBytesPerUser
		}
		if safemath.SumExceedsInt64(maxPending, pending, in.Size) {
			return nil, ErrPendingUploadLimitExceeded
		}
	}

	// 個数の policy も同じく小数を取りうる。int に丸めると 0.5 が 0 になり
	// `limit > 0` を抜けて gate ごと消えるので、float のまま比較する。
	if limit, ok := policyNumber(policies[role.PolicyChunkedUploadMaxConcurrentSessions]); ok && limit > 0 {
		if cap := float64(settings.MaxSessionsPerUser); cap > 0 && limit > cap {
			limit = cap
		}
		active, err := s.chunkedRepo.CountActiveByUser(in.User.ID, now)
		if err != nil {
			return nil, fmt.Errorf("count chunked upload sessions: %w", err)
		}
		if float64(active) >= limit {
			return nil, ErrTooManyUploadSessions
		}
	}

	// 宛先 folder は Upload と同じ扱い: 存在しない場合と他人所有の場合を
	// 区別せず ErrFolderNotFound に倒して存在 oracle を閉じる (#1908)。
	if in.FolderID != nil {
		folder, err := s.folderRepo.FindByID(*in.FolderID)
		if err != nil {
			// **DB 障害を not-found に丸めない** (#2799)。
			if !repository.IsNotFound(err) {
				return nil, err
			}
			return nil, ErrFolderNotFound
		}
		if folder.UserID == nil || *folder.UserID != in.User.ID {
			return nil, ErrFolderNotFound
		}
	}

	// accessKey は既存 upload と同じ生成器を使う。クライアント入力は一切
	// 混ぜないのでパストラバーサルの余地は無い。
	accessKey, err := newAccessKey()
	if err != nil {
		return nil, err
	}

	sess := &model.ChunkedUploadSession{
		ID:        s.idGen.Generate(now),
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(settings.SessionTTL),

		UserID:      in.User.ID,
		Name:        in.Name,
		Comment:     in.Comment,
		FolderID:    in.FolderID,
		IsSensitive: in.IsSensitive,
		Force:       in.Force,

		TotalSize: in.Size,
		// start 時点の値を固定して持つ。進行中に admin が設定を変えても
		// パートサイズが揺れないようにする (R2 は最終パート以外の均一サイズを
		// 要求するので、途中で変わると complete が失敗する)。
		ChunkSize: settings.ChunkSize,
		AccessKey: accessKey,
		Parts:     datatypes.JSON("[]"),

		RequestIP:      in.RequestIP,
		RequestHeaders: in.RequestHeaders,
	}
	if err := s.chunkedRepo.Create(sess); err != nil {
		return nil, fmt.Errorf("create chunked upload session: %w", err)
	}
	return sess, nil
}

// ChunkAppendResult reports the session's progress after an append.
type ChunkAppendResult struct {
	Index         int   `json:"index"`
	Next          int   `json:"next"`
	ReceivedBytes int64 `json:"receivedBytes"`
	TotalSize     int64 `json:"totalSize"`
	Completed     bool  `json:"completed"`
}

// SessionChunkSize reports the chunk size a session accepts, so the handler can
// stop copying past it.
//
// **`io.ReadAll` のコピーを 1 つ減らすためのもの (#3037)。** `AppendChunk` は
// `size > sess.ChunkSize` を拒否するが、そこへ届く時点でチャンクは全部
// 読み終わっている。`drive/files/create` を同じ理由で直したのに、こちらだけ
// 残っていた。
//
// **「読み切る前に落とす」ではない (レビュー 2 周目の実測)。** handler は
// これを呼ぶ前に `c.FormValue("uploadId")` を読み、echo の `FormValue` は
// `http.Request.ParseMultipartForm(32MiB)` を通す。つまり**この関数へ来る
// 時点で body はワイヤから全部読み出され、パーサのメモリに載っている**
// (実測: 5,243,351 バイトの body に対し `FormValue` の直後で
// 5,243,351 バイトが読み出し済み)。`ReadForm` の閾値は 32MiB + 10MiB で
// この route の body limit は 33MiB なので、チャンクは常にメモリに残る。
// 消せているのは `io.ReadAll` による**2 つ目のコピー**だけで、ピークは
// 66MiB から 33MiB に半減するにとどまる。**本当に塞ぐには
// `c.Request().MultipartReader()` でストリーム処理するか、body limit を
// セッション単位にする必要がある。**
//
// (`drive/files/create` 側は 250MB のような大きい本体がパーサで**ディスクへ
// spill** されるので、そちらの guard は最大 250MB のヒープ確保を実際に消す。)
//
// ok=false は「上限を決められない」(セッションが無い / 他人のもの / 期限切れ)。
// **その場合は上限を掛けない** — 本物の判定は `AppendChunk` が行い、そちらが
// 正しいエラーを返す。ここで独自に落とすと、応答の種類が経路で食い違う。
// 存在しない `uploadId` を送ればこの枝に入れるが、body limit が 33MiB なので
// 最悪値は変わらない (`AppendChunk` がその後 404 を返す)。
func (s *Service) SessionChunkSize(user *model.User, sessionID string) (int64, bool) {
	if s.chunkedRepo == nil {
		return 0, false
	}
	sess, err := s.loadOwnedSession(user, sessionID, time.Now())
	if err != nil || sess == nil || sess.ChunkSize <= 0 {
		return 0, false
	}
	return sess.ChunkSize, true
}

// AppendChunk stores one chunk as a part of the session's multipart upload.
//
// index は 0 始まりで、サーバーが受理するのは常に「次の 1 つ」だけ。順序前後・
// 欠番が構造的に起きないようにしている。既に記録済みの index の再送は、内容が
// 一致していれば冪等に成功し、違っていれば ErrChunkContentMismatch で拒否する。
func (s *Service) AppendChunk(ctx context.Context, user *model.User, sessionID string, index int, chunk []byte) (*ChunkAppendResult, error) {
	_, ms, err := s.chunkedMultipart()
	if err != nil {
		return nil, err
	}
	sess, err := s.loadOwnedSession(user, sessionID, time.Now())
	if err != nil {
		return nil, err
	}
	parts, err := decodeChunkedParts(sess.Parts)
	if err != nil {
		return nil, fmt.Errorf("decode chunked upload parts: %w", err)
	}

	// 記録済み index の再送 (= 応答が失われたクライアントの再試行)。
	if index < sess.ReceivedChunks {
		for _, p := range parts {
			if p.Index != index {
				continue
			}
			if p.Size != int64(len(chunk)) || p.SHA256 != sha256Hex(chunk) {
				return nil, ErrChunkContentMismatch
			}
			return &ChunkAppendResult{
				Index:         index,
				Next:          sess.ReceivedChunks,
				ReceivedBytes: sess.ReceivedBytes,
				TotalSize:     sess.TotalSize,
				Completed:     sess.ReceivedBytes == sess.TotalSize,
			}, nil
		}
		return nil, &ChunkIndexError{Expected: sess.ReceivedChunks}
	}
	if index != sess.ReceivedChunks {
		return nil, &ChunkIndexError{Expected: sess.ReceivedChunks}
	}

	size := int64(len(chunk))
	if size <= 0 {
		return nil, ErrInvalidChunkSize
	}
	remaining := sess.TotalSize - sess.ReceivedBytes
	if size > remaining {
		// 申告値を超えた分は受け取らない。これが無いと「1 バイトと申告して
		// 無制限に送る」でサイズ・容量 gate を丸ごと迂回できる。
		return nil, ErrMaxFileSizeExceeded
	}
	if size > sess.ChunkSize {
		// 最終チャンクでも chunkSize は超えさせない。これが無いと「全体を
		// 1 チャンクで送る」が通り、operator が chunkSize を絞った意味
		// (= リバースプロキシの上限に収める) が失われる。
		return nil, ErrInvalidChunkSize
	}
	if size != remaining && size != sess.ChunkSize {
		// 最終チャンク以外は chunkSize ちょうどを要求する。S3 の最小パート
		// サイズと、R2 の「最終パート以外はすべて同一サイズ」を同時に満たす。
		return nil, ErrInvalidChunkSize
	}

	uploadID, err := s.ensureMultipartUpload(ctx, ms, user, sess, chunk)
	if err != nil {
		return nil, err
	}

	// S3 の PartNumber は 1 始まり。
	etag, err := ms.UploadPart(ctx, sess.AccessKey, uploadID, int32(index+1), chunk)
	if err != nil {
		return nil, err
	}

	parts = append(parts, model.ChunkedUploadPart{
		Index:  index,
		ETag:   etag,
		Size:   size,
		SHA256: sha256Hex(chunk),
	})
	encoded, err := json.Marshal(parts)
	if err != nil {
		return nil, fmt.Errorf("encode chunked upload parts: %w", err)
	}
	received := sess.ReceivedBytes + size
	ok, err := s.chunkedRepo.CommitPart(sess.ID, index, encoded, received, time.Now())
	if err != nil {
		return nil, fmt.Errorf("commit chunked upload part: %w", err)
	}
	if !ok {
		// 同じ index を送った別リクエストが先に確定した、あるいは finish が
		// 走り始めた。どちらもクライアント側の再同期で解決できる。
		return nil, &ChunkIndexError{Expected: index + 1}
	}
	return &ChunkAppendResult{
		Index:         index,
		Next:          index + 1,
		ReceivedBytes: received,
		TotalSize:     sess.TotalSize,
		Completed:     received == sess.TotalSize,
	}, nil
}

// ensureMultipartUpload returns the session's backend upload id, creating the
// multipart upload on the first append.
//
// Content-Type はここで確定する。S3Storage.Put と同じく先頭バイトを sniff して
// BrowserSafeContentType に通す — public-read の S3 / CDN 直配信では object の
// Content-Type が描画を支配するため、非 browser-safe を octet-stream に矯正
// しないと CDN ドメイン上の stored XSS になる (#2106 H4)。
func (s *Service) ensureMultipartUpload(ctx context.Context, ms MultipartStorage, user *model.User, sess *model.ChunkedUploadSession, chunk []byte) (string, error) {
	if sess.UploadID != nil {
		return *sess.UploadID, nil
	}
	sniff := chunk
	if len(sniff) > MIMESniffLen {
		sniff = sniff[:MIMESniffLen]
	}
	detected := DetectMIME(sniff)
	// uploadableFileTypes は finish 時の Upload でも効くが、そこまで受け切って
	// から弾くのは無駄なので最初のパートで判定できる時点で落とす。
	if err := s.checkUploadableType(user, detected); err != nil {
		return "", err
	}
	contentType := BrowserSafeContentType(detected)

	uploadID, err := ms.CreateMultipartUpload(ctx, sess.AccessKey, contentType)
	if err != nil {
		return "", err
	}
	// UploadPart より前に記録する。逆順だと途中で失敗したときに DB から
	// 辿れない未完了マルチパートアップロードが残り、GC が abort できない。
	ok, err := s.chunkedRepo.SetUploadID(sess.ID, uploadID, contentType, time.Now())
	if err != nil {
		// 記録できなかった以上こちらの upload は孤児になるので abort する。
		_ = ms.AbortMultipartUpload(ctx, sess.AccessKey, uploadID)
		return "", fmt.Errorf("record chunked upload id: %w", err)
	}
	if !ok {
		// 並行した最初の append が先に確定させた。こちらが作った multipart
		// upload は誰も参照しないので abort する。
		_ = ms.AbortMultipartUpload(ctx, sess.AccessKey, uploadID)
		return "", ErrUploadSessionBusy
	}
	sess.UploadID = &uploadID
	sess.ContentType = &contentType
	return uploadID, nil
}

// checkUploadableType applies the uploadableFileTypes policy, mirroring the
// gate Upload performs on the assembled body.
func (s *Service) checkUploadableType(user *model.User, mime string) error {
	if user == nil || s.roleChecker == nil {
		return nil
	}
	if s.roleChecker.IsModerator(user.ID) {
		return nil
	}
	policies := s.roleChecker.GetUserPolicies(user.ID)
	if policies == nil {
		return nil
	}
	if !mimeAllowedByPolicy(mime, policies["uploadableFileTypes"]) {
		return ErrUnallowedFileType
	}
	return nil
}

// FinishChunkedUpload completes the multipart upload and creates the DriveFile.
//
// 組み上がった実体を storage から読み戻してから既存の Upload 経路に通す。
// サムネイル生成 (generateAlts) とセンシティブ判定 (detectSensitive) はどちらも
// ボディ全体を要求するので、読み戻さないと**大きい動画だけサムネイルが出ず、
// 自動判定も効かない**という機能欠落になる。読み戻しは finish の一瞬だけで、
// 転送中はサーバー側にバイトを置かないという利点は保たれる。
func (s *Service) FinishChunkedUpload(ctx context.Context, user *model.User, sessionID string) (*model.DriveFile, error) {
	_, ms, err := s.chunkedMultipart()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess, err := s.loadOwnedSession(user, sessionID, now)
	if err != nil {
		return nil, err
	}
	if sess.UploadID == nil || sess.ReceivedBytes != sess.TotalSize {
		return nil, ErrIncompleteUpload
	}
	parts, err := decodeChunkedParts(sess.Parts)
	if err != nil {
		return nil, fmt.Errorf("decode chunked upload parts: %w", err)
	}
	uploaded, err := orderedParts(parts, sess.ReceivedChunks, sess.ReceivedBytes)
	if err != nil {
		return nil, err
	}

	// finish の同時実行で DriveFile が二重作成されないよう、1 本だけ通す。
	claimed, err := s.chunkedRepo.ClaimFinish(sess.ID, now)
	if err != nil {
		return nil, fmt.Errorf("claim chunked upload finish: %w", err)
	}
	if !claimed {
		return nil, ErrUploadSessionBusy
	}

	url, err := ms.CompleteMultipartUpload(ctx, sess.AccessKey, *sess.UploadID, uploaded)
	if err != nil {
		// まだ object は出来ていない。claim を戻してリトライ可能にする
		// (セッション行を消すと未完了マルチパートアップロードが追跡不能になる)。
		_ = s.chunkedRepo.ReleaseFinish(sess.ID, time.Now())
		return nil, err
	}

	// ここから先は storage 上に object が確定している。以降どの経路で
	// 失敗しても消してからセッションを畳む。
	body, err := s.readBackChunkedObject(sess)
	if err != nil {
		s.discardChunkedObject(sess)
		return nil, err
	}

	f, err := s.Upload(ctx, UploadInput{
		User:           user,
		Body:           body,
		Name:           sess.Name,
		Comment:        sess.Comment,
		FolderID:       sess.FolderID,
		IsSensitive:    sess.IsSensitive,
		Force:          sess.Force,
		RequestIP:      sess.RequestIP,
		RequestHeaders: sess.RequestHeaders,
		PreStored:      &PreStoredObject{AccessKey: sess.AccessKey, URL: url},
	})
	if err != nil {
		s.discardChunkedObject(sess)
		return nil, err
	}
	// 重複排除で既存ファイルが返った場合、いま組み上げた object は誰からも
	// 参照されない。放置すると課金だけ残るので消す。
	if f.AccessKey == nil || *f.AccessKey != sess.AccessKey {
		_ = s.storage.Delete(sess.AccessKey)
	}
	if err := s.chunkedRepo.Delete(sess.ID); err != nil {
		slog.Warn("chunked upload: delete session failed", "sessionId", sess.ID, "err", err)
	}
	return f, nil
}

// readBackChunkedObject fetches the assembled object and verifies its length
// against what the session recorded.
func (s *Service) readBackChunkedObject(sess *model.ChunkedUploadSession) ([]byte, error) {
	rc, err := s.storage.Get(sess.AccessKey)
	if err != nil {
		return nil, fmt.Errorf("read back chunked upload: %w", err)
	}
	defer rc.Close()
	// TotalSize+1 まで読んで、超過していれば取り違えとして扱う。読み取り量は
	// start 時点で maxFileSizeMb によって bound されている。
	body, err := io.ReadAll(io.LimitReader(rc, sess.TotalSize+1))
	if err != nil {
		return nil, fmt.Errorf("read back chunked upload: %w", err)
	}
	if int64(len(body)) != sess.TotalSize {
		return nil, ErrIncompleteUpload
	}
	return body, nil
}

// discardChunkedObject removes the assembled object and the session row after a
// failed finish. Best-effort: both are logged rather than surfaced, since the
// caller is already returning an error.
func (s *Service) discardChunkedObject(sess *model.ChunkedUploadSession) {
	if err := s.storage.Delete(sess.AccessKey); err != nil {
		slog.Warn("chunked upload: delete assembled object failed", "accessKey", sess.AccessKey, "err", err)
	}
	if err := s.chunkedRepo.Delete(sess.ID); err != nil {
		slog.Warn("chunked upload: delete session failed", "sessionId", sess.ID, "err", err)
	}
}

// AbortChunkedUpload discards a session and its multipart upload.
func (s *Service) AbortChunkedUpload(ctx context.Context, user *model.User, sessionID string) error {
	if s.chunkedRepo == nil {
		return ErrChunkedUploadUnavailable
	}
	// abort は期限切れでも通す: クライアントが後片付けしようとしているのを
	// 拒む理由が無く、通した方が S3 の未完了アップロードが早く消える。
	sess, err := s.loadOwnedSession(user, sessionID, time.Time{})
	if err != nil {
		return err
	}
	if sess.UploadID != nil {
		if ms, ok := ResolveStorage(s.storage).(MultipartStorage); ok {
			if err := ms.AbortMultipartUpload(ctx, sess.AccessKey, *sess.UploadID); err != nil {
				// abort できないまま行を消すと未完了アップロードが追跡不能に
				// なるので、行は残して GC に再試行させる。
				return err
			}
		}
	}
	return s.chunkedRepo.Delete(sess.ID)
}

// GCChunkedUploads aborts and removes expired sessions, returning how many were
// reclaimed. Object storage bills for incomplete multipart uploads, so this
// must run regularly; operators should also set a bucket lifecycle rule for
// incomplete multipart uploads as a backstop for when mk-go's GC is not running.
func (s *Service) GCChunkedUploads(ctx context.Context, now time.Time, limit int) (int, error) {
	if s.chunkedRepo == nil {
		return 0, nil
	}
	sessions, err := s.chunkedRepo.ListExpired(now, limit)
	if err != nil {
		return 0, err
	}
	ms, hasMultipart := ResolveStorage(s.storage).(MultipartStorage)
	reclaimed := 0
	for _, sess := range sessions {
		// finish は期限内に始まっていれば期限後も走り続ける。その最中に
		// AbortMultipartUpload を撃つと、正常な完了を横から壊してしまう。
		// 直近まで動いていた finish は猶予を与えて次回に回す (finish が
		// 落ちたまま放置された場合は猶予経過後に回収される)。
		if sess.Finishing && sess.UpdatedAt.After(now.Add(-finishGracePeriod)) {
			continue
		}
		if sess.UploadID != nil {
			switch {
			case !hasMultipart:
				// storage 設定が S3 からローカルに戻された等で abort する手段が
				// 無い。行を残しても永久に処理できないので消すが、バケット側の
				// ライフサイクルルールでの回収が必要なことを警告に残す。
				slog.Warn("chunked upload: cannot abort multipart upload, storage backend no longer supports it",
					"sessionId", sess.ID, "accessKey", sess.AccessKey)
			default:
				if err := ms.AbortMultipartUpload(ctx, sess.AccessKey, *sess.UploadID); err != nil {
					// 消さずに残して次回再試行する。
					slog.Warn("chunked upload: abort multipart upload failed",
						"sessionId", sess.ID, "accessKey", sess.AccessKey, "err", err)
					continue
				}
			}
		}
		if err := s.chunkedRepo.Delete(sess.ID); err != nil {
			slog.Warn("chunked upload: delete expired session failed", "sessionId", sess.ID, "err", err)
			continue
		}
		reclaimed++
	}
	return reclaimed, nil
}

// loadOwnedSession fetches a session owned by user.
//
// 見つからない / 他人のもの / 期限切れ をすべて ErrUploadSessionNotFound に
// 畳んで返す。区別すると他人のセッション ID の存在を確かめる oracle になる。
// now がゼロ値のときは期限を見ない (abort 経路)。
func (s *Service) loadOwnedSession(user *model.User, sessionID string, now time.Time) (*model.ChunkedUploadSession, error) {
	if s.chunkedRepo == nil {
		return nil, ErrChunkedUploadUnavailable
	}
	if user == nil || sessionID == "" {
		return nil, ErrUploadSessionNotFound
	}
	sess, err := s.chunkedRepo.FindByID(sessionID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrUploadSessionNotFound
	}
	if sess.UserID != user.ID {
		return nil, ErrUploadSessionNotFound
	}
	if !now.IsZero() && !sess.ExpiresAt.After(now) {
		return nil, ErrUploadSessionNotFound
	}
	return sess, nil
}

// decodeChunkedParts unmarshals the session's recorded parts.
func decodeChunkedParts(raw datatypes.JSON) ([]model.ChunkedUploadPart, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var parts []model.ChunkedUploadPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// orderedParts validates that the recorded parts form a contiguous 0..count-1
// run summing to totalBytes, and converts them to backend part references.
//
// append 側で順序を強制しているので通常は成立するが、finish は「壊れたものを
// 組み上げない」最後の砦なのでここでも検証する。
func orderedParts(parts []model.ChunkedUploadPart, count int, totalBytes int64) ([]UploadedPart, error) {
	if len(parts) != count || count == 0 {
		return nil, ErrIncompleteUpload
	}
	out := make([]UploadedPart, count)
	seen := make([]bool, count)
	var sum int64
	for _, p := range parts {
		if p.Index < 0 || p.Index >= count || seen[p.Index] {
			return nil, ErrIncompleteUpload
		}
		seen[p.Index] = true
		sum += p.Size
		out[p.Index] = UploadedPart{PartNumber: int32(p.Index + 1), ETag: p.ETag}
	}
	if sum != totalBytes {
		return nil, ErrIncompleteUpload
	}
	return out, nil
}

// sha256Hex returns the hex-encoded SHA-256 of b. Recorded per part so a retried
// append carrying different bytes is rejected instead of silently changing the
// assembled file.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
