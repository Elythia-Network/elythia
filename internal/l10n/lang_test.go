package l10n

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestResolve_ProfileLang(t *testing.T) {
	ja := "ja-JP"
	assert.Equal(t, "ja", Resolve(&ja, []string{"en-US"}))
}

func TestResolve_MetaLangsFallback(t *testing.T) {
	assert.Equal(t, "ja", Resolve(nil, []string{"ja-JP"}))
}

func TestResolve_BilingualWhenNoKnownLocale(t *testing.T) {
	assert.Equal(t, LangBilingual, Resolve(nil, nil))
	unknown := "fr-FR"
	assert.Equal(t, LangBilingual, Resolve(&unknown, []string{"de-DE"}))
}

func TestResolveFromHeader_MatchesInstanceLang(t *testing.T) {
	got := ResolveFromHeader("ja-JP,en;q=0.8", []string{"en-US", "ja-JP"})
	assert.Equal(t, "ja", got)
}

func TestResolveFromHeader_FallsBackToMeta(t *testing.T) {
	got := ResolveFromHeader("fr-FR", []string{"ja-JP"})
	assert.Equal(t, "ja", got)
}

func TestResolveFromHeader_SkipsWildcardAndUnknown(t *testing.T) {
	got := ResolveFromHeader("*,fr-FR,en-US", []string{"en-US"})
	assert.Equal(t, "en", got)
}

func TestResolveFromHeader_EmptyHeaderUsesMeta(t *testing.T) {
	assert.Equal(t, "ja", ResolveFromHeader("", []string{"ja-JP"}))
}

func TestResolveFromHeader_UsesHeaderWhenMetaLangsEmpty(t *testing.T) {
	assert.Equal(t, "ja", ResolveFromHeader("ja-JP,en;q=0.8", nil))
	assert.Equal(t, LangBilingual, ResolveFromHeader("", nil))
}

func TestLangsFromMeta(t *testing.T) {
	assert.Nil(t, LangsFromMeta(nil))
	meta := &model.Meta{Langs: []string{"ja-JP", "en-US"}}
	assert.Equal(t, []string{"ja-JP", "en-US"}, LangsFromMeta(meta))
}
