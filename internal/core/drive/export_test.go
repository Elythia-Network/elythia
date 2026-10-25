package drive

import (
	"context"
	"image"
	"io"
	"os"
)

// SetRandReaderForTest replaces the package-level random source for tests.
// 戻り値は元のreaderで、テスト終了時に呼び出して復元する。
func SetRandReaderForTest(r io.Reader) func() {
	prev := randReader
	randReader = r
	return func() { randReader = prev }
}

// SetWebPEncoderForTest replaces the WebP encoder for testing error paths.
func SetWebPEncoderForTest(f func(image.Image, int) ([]byte, error)) func() {
	prev := webpEncoderFunc
	webpEncoderFunc = f
	return func() { webpEncoderFunc = prev }
}

// SetPNGEncoderForTest replaces the PNG encoder for testing error paths.
func SetPNGEncoderForTest(f func(image.Image) ([]byte, error)) func() {
	prev := pngEncoderFunc
	pngEncoderFunc = f
	return func() { pngEncoderFunc = prev }
}

// SetOsMkdirTempForTest replaces os.MkdirTemp for testing error paths.
func SetOsMkdirTempForTest(f func(string, string) (string, error)) func() {
	prev := osMkdirTemp
	osMkdirTemp = f
	return func() { osMkdirTemp = prev }
}

// SetOsWriteFileForTest replaces os.WriteFile for testing error paths.
func SetOsWriteFileForTest(f func(string, []byte, os.FileMode) error) func() {
	prev := osWriteFile
	osWriteFile = f
	return func() { osWriteFile = prev }
}

// GenerateAltsForTest exposes generateAlts for external tests.
func (s *Service) GenerateAltsForTest(body []byte, mimeType string) (
	thumbnail *ProcessedImage,
	webpublic *ProcessedImage,
	blurhash *string,
) {
	result := s.generateAlts(context.Background(), body, mimeType)
	if result == nil {
		return nil, nil, nil
	}
	return result.thumbnail, result.webpublic, result.blurhash
}

// MimeAllowedByPolicyForTest exposes mimeAllowedByPolicy so external tests
// can exercise the helper's pattern-matching logic directly (#1028).
func MimeAllowedByPolicyForTest(mime string, raw any) bool {
	return mimeAllowedByPolicy(mime, raw)
}

// GenerateAltsCtxForTest exposes generateAlts with an explicit context so
// external tests can exercise the concurrency slot and its cancellation path.
func (s *Service) GenerateAltsCtxForTest(ctx context.Context, body []byte, mimeType string) bool {
	return s.generateAlts(ctx, body, mimeType) != nil
}

// MediaProcessingSlotsForTest reports the configured processing pool size.
func (s *Service) MediaProcessingSlotsForTest() int { return s.mediaSlotCap }

// DefaultMediaProcessingConcurrencyForTest exposes the default pool size.
func DefaultMediaProcessingConcurrencyForTest() int { return defaultMediaProcessingConcurrency() }

// SetRoleCheckerForTest replaces the role checker after construction. 本番は
// NewService の必須の引数で渡す (#3825)。外部パッケージのテストが補助関数で作った
// service に後から入れるための口で、テストのビルドでだけ見える。
func (s *Service) SetRoleCheckerForTest(rc RoleChecker) { s.roleChecker = rc }

// SetModerationLogForTest replaces the moderation log dependencies after
// construction (see SetRoleCheckerForTest).
func (s *Service) SetModerationLogForTest(l ModerationLogger, users ModerationLogUserLookup) {
	s.modLog = l
	s.modLogUsers = users
}
