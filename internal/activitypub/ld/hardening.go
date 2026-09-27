package ld

import (
	"errors"
	"fmt"
)

// 2026.5.4 hardening 関連 sentinel errors。
var (
	// ErrCacheOverflow is returned when the per-verify context cache would
	// exceed maxCacheSize. Mirrors upstream JsonLdCacheOverflowError
	// (UUID `42fb039c-69fb-4f75-8187-d3aee412423e`)。
	ErrCacheOverflow = errors.New("ld-sig: jsonld context cache overflow")
	// ErrCacheFrozen is returned when a context fetch is attempted after
	// Freeze(). Mirrors upstream JsonLdCacheFrozenError
	// (UUID `202c41fa-72d5-4e22-95af-94a8ac83346f`)。
	ErrCacheFrozen = errors.New("ld-sig: attempt to fetch context after freeze")
	// ErrForbiddenDirective is returned when an inbound activity contains a
	// JSON-LD directive Misskey refuses to canonicalize (= upstream 2026.5.4
	// fix で `@included` / `@graph` / `@reverse` を弾く挙動と等価)。
	// upstream UUID `0297f79b-0ed9-4b6c-875f-b0a82ff96781`。
	ErrForbiddenDirective = errors.New("ld-sig: directive is forbidden by Misskey in ActivityPub documents")
)

// defaultCacheCap matches upstream JsonLd `if (this.cache.size > 256) throw`.
const defaultCacheCap = 256

// forbiddenDirectives matches upstream `JsonLd.forbiddenDirectives` set.
// これらは canonicalize-time に意味変換を引き起こす危険 directive。詳細:
//   - `@reverse`: subject/object を入れ替えて normalize 結果を変える
//   - `@graph` / `@included`: 複数 sub-document をまとめて wrapper signature
//     で内側 payload を後付け差し替え可能にする
var forbiddenDirectives = map[string]struct{}{
	"@included": {},
	"@graph":    {},
	"@reverse":  {},
}

// CheckForForbiddenDirectives walks the supplied document recursively and
// returns ErrForbiddenDirective when an upstream-forbidden JSON-LD directive
// is encountered. Mirrors upstream `JsonLd.checkForForbiddenDirectives`。
//
// Caller (= InboxProcessor) は LD-Signature verify の前に必ず本関数を通すこと。
// upstream の順序は compact → CheckForForbiddenDirectives → Freeze →
// VerifyRsaSignature2017。mk-go の受信経路 (federation.LDSignatureVerifier) は
// fetch しないので Freeze を compact の前に置き、生の文書と compact 後の文書の
// 両方に本関数を掛ける。
func (p *Processor) CheckForForbiddenDirectives(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			if _, bad := forbiddenDirectives[k]; bad {
				return fmt.Errorf("%w: %s", ErrForbiddenDirective, k)
			}
			if err := p.CheckForForbiddenDirectives(child); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := p.CheckForForbiddenDirectives(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// Freeze prevents the processor from fetching any further context documents.
// Caller は LD-Signature verify を呼ぶ直前に呼ぶこと (= SSRF + TOCTOU race の
// 遮断)。Freeze 後の loadDocument 呼び出しは ErrCacheFrozen を返す。
func (p *Processor) Freeze() {
	p.mu.Lock()
	p.frozen = true
	p.mu.Unlock()
}
