package drive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3API abstracts the subset of S3 client operations used by S3Storage.
// テスト時はモックに差し替え可能。
type S3API interface {
	PutObject(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, input *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, input *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	// Multipart upload (#2313)。S3Storage が MultipartStorage を満たすために必要。
	CreateMultipartUpload(ctx context.Context, input *s3.CreateMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(ctx context.Context, input *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(ctx context.Context, input *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, input *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// S3Storage implements Storage using an S3-compatible object storage backend.
//
// prefix is normalised at construction so that operators can enter either
// "files" or "files/" in the admin UI; objectKey always emits
// "<prefix>/<accessKey>". This matches Misskey TS which always joins prefix
// and accessKey with a "/" (#525).
type S3Storage struct {
	client        S3API
	bucket        string
	prefix        string // 末尾 / を取り除いた状態で保持。空なら prefix なし。
	baseURL       string // 公開 URL のベース (e.g. "https://cdn.example.com")
	setPublicRead bool
}

// S3StorageConfig holds the configuration for creating an S3Storage.
type S3StorageConfig struct {
	Client        S3API
	Bucket        string
	Prefix        string
	BaseURL       string // 空の場合はエンドポイントから自動生成
	SetPublicRead bool
}

// NewS3Storage creates a new S3Storage instance.
func NewS3Storage(cfg S3StorageConfig) *S3Storage {
	return &S3Storage{
		client: cfg.Client,
		bucket: cfg.Bucket,
		// Misskey TS の admin UI は placeholder が "files" (末尾 / 無し) で、
		// 既存 operator はそのままの値を DB に入れている。一方で末尾 / 付きで
		// 入れている運用も実在するので、ここで一度だけ正規化して保持する。
		// "files/" でも "files" でも、最終的な objectKey は
		// "files/<accessKey>" に揃う (#525)。
		prefix:        strings.TrimRight(cfg.Prefix, "/"),
		baseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		setPublicRead: cfg.SetPublicRead,
	}
}

// objectKey returns the full S3 object key for the given accessKey.
// Empty prefix returns just accessKey; otherwise emits "<prefix>/<accessKey>".
func (s *S3Storage) objectKey(accessKey string) string {
	// prefix と accessKey の間に必ず "/" を入れる。Misskey TS の objectKey
	// 生成 (`${prefix}/${accessKey}`) と一致させて、同じ DB に対する
	// drop-in 切替で互いの書き込みが読めるようにする (#525)。
	if s.prefix == "" {
		return accessKey
	}
	return s.prefix + "/" + accessKey
}

// publicURL returns the public URL for the given accessKey.
func (s *S3Storage) publicURL(accessKey string) string {
	key := s.objectKey(accessKey)
	if s.baseURL != "" {
		return s.baseURL + "/" + key
	}
	// baseURL が未設定の場合は S3 パススタイル URL にフォールバック
	return fmt.Sprintf("https://s3.amazonaws.com/%s/%s", s.bucket, key)
}

// Put uploads the body to S3 and returns the public URL.
//
// aws-sdk-go-v2 は SigV4 payload hash を計算するため Body が io.Seeker
// を満たす必要がある。以前は MIME 判定のために `io.MultiReader` で wrap
// していたが、MultiReader は Seeker を実装しないので SDK が
// "failed to seek body to start, request stream is not seekable" で即 fail し、
// HTTP リクエストすら送出されず drive/files/create が常に 500 になっていた
// (#523)。一度 []byte に集めて bytes.Reader (Seeker 実装) を渡すことで解消。
func (s *S3Storage) Put(accessKey string, body io.Reader) (string, error) {
	key := s.objectKey(accessKey)

	data, err := io.ReadAll(body)
	if err != nil {
		return "", fmt.Errorf("s3 read body for %s: %w", key, err)
	}
	sniff := data
	if len(sniff) > MIMESniffLen {
		sniff = sniff[:MIMESniffLen]
	}
	// #2106 H4: sniff した MIME を browser-safe allowlist に通す。public-read S3/CDN
	// 直配信では /files ハンドラ・media proxy を両方バイパスして object の Content-Type が
	// 描画を支配するため、非 browser-safe (HTML/SVG/XML 等) を octet-stream に矯正
	// しないと CDN ドメイン上で stored XSS になる (upstream DriveService と同等)。
	contentType := BrowserSafeContentType(DetectMIME(sniff))

	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
		// octet-stream 矯正と併せ Content-Disposition: inline を付け、active content の
		// インライン実行をさらに抑止する (upstream FileServerUtils.setFileResponseHeaders 相当)。
		ContentType:        aws.String(contentType),
		ContentDisposition: aws.String("inline"),
		CacheControl:       aws.String("max-age=31536000, immutable"),
	}
	if s.setPublicRead {
		input.ACL = types.ObjectCannedACLPublicRead
	}

	if _, err := s.client.PutObject(context.Background(), input); err != nil {
		return "", fmt.Errorf("s3 put %s: %w", key, err)
	}
	return s.publicURL(accessKey), nil
}

// Get retrieves an object from S3 and returns an io.ReadCloser.
//
// **本当に「無い」ときだけ ErrObjectNotFound を返す (#2990)。** 以前は
// `GetObject` の失敗を種別を問わず ErrObjectNotFound に潰していたので、
// 認証切れ・throttling・endpoint 誤設定・provider の 5xx・接続断がすべて
// 「そんなオブジェクトは無い」に化けていた。#2792 が禁じている形そのもので、
// 呼び出し側は 3 つとも危険な判断をする — media proxy は 404 を返して
// 監視に 5xx が立たず、絵文字申請の承認は 400 `NO_SUCH_FILE` を返して
// モデレーターには却下すべき申請に見え、#2990 のバッチは「元画像を復元できない
// ので絵文字を消すか差し替えろ」と案内する。
func (s *S3Storage) Get(accessKey string) (io.ReadCloser, error) {
	key := s.objectKey(accessKey)
	output, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	// 大きさを読む前に分かるようにする (ObjectSize)。
	if output.ContentLength != nil && *output.ContentLength >= 0 {
		return &sizedBody{ReadCloser: output.Body, size: *output.ContentLength}, nil
	}
	return output.Body, nil
}

// isS3NotFound reports whether err means the object genuinely is not there.
//
// **読めているエラーコードを最優先する。** S3 本家と互換実装 (MinIO / R2 / Wasabi /
// Ceph) はどれも 404 + `<Code>NoSuchKey</Code>` を返す。`NotFound` のほうは
// **SDK が status から作るコード** — 本文に `<Code>` も `<Message>` も無い 404 だと
// `s3shared` の `UseStatusCode` が `http.StatusText(404)` から `NotFound` を組み立てる
// (本文が空 / plain text の互換実装で実際に起きる)。
//
// **`NoSuchBucket` も HTTP 404 だが「無い」ではない** — バケット名の設定誤りで、
// これを not-found に倒すと **1 文字の typo で全オブジェクトが「消えた」と判定
// される** (#2990 のバッチはそれを「絵文字を消すか差し替えろ」と案内する)。
// コードが読めているなら、知らないコードも「無い」とは断定しない。
//
// **`s3:ListBucket` 権限が無いバケットは対象外。** あの構成では存在しないキーへの
// `GetObject` が 403 `AccessDenied` になり、ここでは障害として扱う。S3 自身が存在の
// 有無を隠しているので「無い」と断定できず、fail-closed に倒すしかない。
//
// **HTTP status に頼るのはコードが読めないときだけ。** endpoint がまったく別の
// HTTP サーバーを指していると `smithy.APIError` にすらならないので、その場合だけ
// 404 を「無い」と見なす。
//
// **`*types.NoSuchKey` を名指しでは見ない。** あれも `smithy.APIError` を満たし
// `ErrorCode() == "NoSuchKey"` を返すので、名指しの分岐は一度も実行されない
// (変異検証で素通りすることを確認した)。
func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		case "":
			// **この deserializer では起きない** (必ず何かのコードが入る) が、
			// `S3API` を差し替えた実装が空で返す余地はあるので status へ抜ける。
		default:
			return false
		}
	}
	var respErr *awshttp.ResponseError
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound
}

// Delete removes an object from S3. Missing objects are silently ignored.
func (s *S3Storage) Delete(accessKey string) error {
	key := s.objectKey(accessKey)
	_, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	return nil
}
