// Package l10n resolves locale tags for outbound email copy.
package l10n

import (
	"strings"

	"github.com/elythia-network/elythia/internal/model"
)

// LangBilingual is used when neither profile.lang nor meta.langs yields a known
// locale. Templates that upstream sent bilingually (new-login, moderator mail)
// keep both languages; others fall back to English.
const LangBilingual = "bilingual"

// Resolve picks the email locale from a user profile lang, then instance langs,
// then LangBilingual when no known locale is available.
func Resolve(profileLang *string, metaLangs []string) string {
	if profileLang != nil && *profileLang != "" {
		if lang := normalizeKnown(*profileLang); lang != "" {
			return lang
		}
	}
	for _, l := range metaLangs {
		if lang := normalizeKnown(l); lang != "" {
			return lang
		}
	}
	return LangBilingual
}

// ResolveFromHeader picks a locale from Accept-Language. When meta.langs is
// non-empty, only tags declared on the instance match. When meta.langs is
// empty, the first known tag in the header wins. Falls back to Resolve(nil,
// metaLangs) when the header has no usable tag.
//
// Accept-Language の q= は読み落とし、ヘッダの並び順で採用する。主要 UA は優先順に
// 並べるので実害は小さい。RFC 7231 の quality 並べ替えが要るならここに足す。
func ResolveFromHeader(acceptLanguage string, metaLangs []string) string {
	if acceptLanguage != "" {
		for _, part := range strings.Split(acceptLanguage, ",") {
			tag := strings.TrimSpace(strings.Split(part, ";")[0])
			if tag == "" || tag == "*" {
				continue
			}
			want := normalizeKnown(tag)
			if want == "" {
				continue
			}
			if len(metaLangs) == 0 {
				return want
			}
			for _, ml := range metaLangs {
				if normalizeKnown(ml) == want {
					return want
				}
			}
		}
	}
	return Resolve(nil, metaLangs)
}

// LangsFromMeta returns meta.langs when meta is non-nil.
func LangsFromMeta(meta *model.Meta) []string {
	if meta == nil {
		return nil
	}
	return meta.Langs
}

func normalizeKnown(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	switch tag {
	case "ja":
		return "ja"
	case "en":
		return "en"
	default:
		return ""
	}
}
