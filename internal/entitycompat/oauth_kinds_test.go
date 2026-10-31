package entitycompat

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// requireScopeRe extracts the scope string from middleware.RequireScope("...").
var requireScopeRe = regexp.MustCompile(`RequireScope\("([^"]+)"\)`)

// The calls the reverse kind check looks for, whatever the arguments are.
var (
	requireScopeCallRe   = regexp.MustCompile(`\bRequireScope\(`)
	admitAppUserCallRe   = regexp.MustCompile(`\bAdmitAppUser\(`)
	rejectAppTokenCallRe = regexp.MustCompile(`\b(?:RejectAppToken|RequireSecure)\(`)
)

// noKindAppTokenAllowed lists endpoints where upstream declares no meta.kind
// and requires a credential (so it rejects every app token), but Elythia's
// route deliberately lets app tokens through. The value is the reason.
var noKindAppTokenAllowed = map[string]string{}

// kindMissingScope lists endpoints where Misskey declares meta.kind but mk-go's
// route has no RequireScope, so an app token can drive them without holding the
// scope. **These are pre-existing gaps, not intentional divergence** — the gate
// exists to stop new ones while these are worked off separately.
//
// 値は「upstream が要求する kind」。潰したらこの表から消す。
// **現在は空 = 全件 upstream の kind どおりに RequireScope が付いている。**
var kindMissingScope = map[string]string{}

// TestOAuthKindDrift checks that mk-go's middleware.RequireScope matches
// Misskey's meta.kind for every endpoint both sides implement.
//
// TestPermissionDrift only compares the coarse public|auth|moderator|admin
// ladder, so a wrong OAuth scope slips through it entirely. That is not
// hypothetical: `admin/queue/stats` shipped with `read:admin:emoji` (copied from
// upstream's own typo, fixed upstream in 2026.9.0) and nothing caught it — an
// app holding only the emoji scope could read queue stats, while an app holding
// the queue scope could not.
func TestOAuthKindDrift(t *testing.T) {
	// 本家の meta.kind は upstream_paramdefs.json の meta から作る (upstreamMetas)。
	metas := upstreamMetas(t)
	golden := upstreamOAuthKinds(metas)
	// 拾えなくなったら落とす。upstream の書式が変わって空になると、
	// 検査していないのに緑になる。`i` は正規表現で読んでいたころ、本家だけが
	// ダブルクォートで書いていて抜け落ちた endpoint なので名指しで要求する。
	if len(golden) < 200 || golden["i"] != "read:account" || golden["admin/queue/stats"] != "read:admin:queue" {
		t.Fatalf("upstream oauth kinds look broken (%d entries, i=%q, admin/queue/stats=%q); regenerate with `make paramdef-gen`",
			len(golden), golden["i"], golden["admin/queue/stats"])
	}

	regs := parseRouteRegistrations(t, serverSourcePaths(t)...)
	if len(regs) < 200 {
		t.Fatalf("parsed only %d routes from internal/server (expected >=200); the parser likely broke", len(regs))
	}

	var mismatched, missing, staleAllow, scopedNoKind []string
	// **逆向きも見る (#3369)。** 本家は kind が無く資格情報を要する endpoint に、
	// app token を一律で拒否する (ApiCallService.ts の `!ep.meta.kind &&
	// (requireCredential || requireModerator || requireAdmin)`)。Elythia の route は
	// そこで app token を拒む middleware (RejectAppToken か RequireSecure) を付け、
	// app token を通すもの (RequireScope、AdmitAppUser) を付けないこと。どれも
	// 付けないと、scope を問わず app token が handler に届く (RequireAuth などは
	// admitHeldAppUser を呼ぶため)。
	var appTokenNotRejected, staleNoKindAllow []string
	seenAllow := map[string]bool{}
	noKindChecked, sawChangePassword := 0, false
	for ep, m := range metas {
		if m.Kind != nil || excludedEndpoint(ep) ||
			(!isTrue(m.RequireCredential) && !isTrue(m.RequireModerator) && !isTrue(m.RequireAdmin)) {
			continue
		}
		reg, ok := regs[ep]
		if !ok {
			continue // not implemented
		}
		noKindChecked++
		sawChangePassword = sawChangePassword || ep == "i/change-password"
		// 識別子を渡す形 (RequireScope(kind)) も拾うよう、呼び出しの有無で見る。
		scoped := requireScopeCallRe.MatchString(reg)
		admits := admitAppUserCallRe.MatchString(reg)
		rejects := rejectAppTokenCallRe.MatchString(reg)
		if _, allowed := noKindAppTokenAllowed[ep]; allowed {
			seenAllow[ep] = true
			if !scoped && !admits && rejects {
				staleNoKindAllow = append(staleNoKindAllow, ep+" (the route rejects app tokens now)")
			}
			continue
		}
		switch {
		case scoped:
			scopedNoKind = append(scopedNoKind, ep+" (mk route has RequireScope)")
		case admits:
			scopedNoKind = append(scopedNoKind, ep+" (mk route has AdmitAppUser)")
		case !rejects:
			appTokenNotRejected = append(appTokenNotRejected, ep)
		}
	}
	for ep := range noKindAppTokenAllowed {
		if !seenAllow[ep] {
			staleNoKindAllow = append(staleNoKindAllow, ep+" (upstream declares a kind or no credential, or Elythia does not implement it)")
		}
	}
	// 拾えなくなったら落とす。i/change-password は kind を持たない secure の endpoint。
	if noKindChecked < 20 || !sawChangePassword {
		t.Fatalf("checked only %d credential endpoints without meta.kind (expected >=20, including i/change-password)", noKindChecked)
	}
	for ep, want := range golden {
		if excludedEndpoint(ep) {
			continue
		}
		reg, ok := regs[ep]
		if !ok {
			continue // not implemented
		}
		m := requireScopeRe.FindStringSubmatch(reg)
		if m == nil {
			if _, allowed := kindMissingScope[ep]; !allowed {
				missing = append(missing, ep+" (upstream kind: "+want+")")
			}
			continue
		}
		if _, allowed := kindMissingScope[ep]; allowed {
			staleAllow = append(staleAllow, ep)
		}
		if m[1] != want {
			mismatched = append(mismatched, ep+": misskey="+want+" mk="+m[1])
		}
	}

	fail := func(head string, list []string) {
		sort.Strings(list)
		var b strings.Builder
		b.WriteString(head + "\n")
		for _, l := range list {
			b.WriteString("  " + l + "\n")
		}
		t.Error(b.String())
	}
	if len(mismatched) > 0 {
		fail("oauth kind drift: mk-go's RequireScope does not match Misskey's meta.kind.", mismatched)
	}
	if len(scopedNoKind) > 0 {
		fail("oauth kind drift: Misskey declares no meta.kind (app tokens are always rejected) but mk-go's route lets an app token through.\nReplace it with RejectAppToken or RequireSecure, or register the reason in noKindAppTokenAllowed.", scopedNoKind)
	}
	if len(appTokenNotRejected) > 0 {
		fail("oauth kind drift: Misskey declares no meta.kind (app tokens are always rejected) but mk-go's route has neither RejectAppToken nor RequireSecure.\nAdd one of them, or register the reason in noKindAppTokenAllowed.", appTokenNotRejected)
	}
	if len(missing) > 0 {
		fail("oauth kind drift: Misskey declares meta.kind but mk-go's route has no RequireScope (app token bypasses the scope).\nFix the route, or register it in kindMissingScope with the upstream kind.", missing)
	}
	// allowlist が実態より広いと、直したものを検出し続けたうえで新しい欠落を隠す。
	if len(staleNoKindAllow) > 0 {
		fail("noKindAppTokenAllowed is stale. Remove these entries.", staleNoKindAllow)
	}
	if len(staleAllow) > 0 {
		fail("kindMissingScope is stale: these endpoints now have RequireScope. Remove them from the allowlist.", staleAllow)
	}
}
