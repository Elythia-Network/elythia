package entitycompat

import (
	"sort"
	"strings"
	"testing"
)

// rolePolicyStricterThanUpstream lists endpoints where Elythia's route does
// not apply upstream's requiredRolePolicy because it guards the endpoint more
// strictly. The value is the reason.
var rolePolicyStricterThanUpstream = map[string]string{}

// TestRolePolicyDrift checks that every endpoint upstream gates with
// meta.requiredRolePolicy applies middleware.RequireRolePolicy with the same
// policy on Elythia's route.
//
// TestPermissionDrift は requiredRolePolicy を段階に数えない (本家の段階は
// requireCredential までで、policy は別の軸)。route から RequireRolePolicy を
// 外しても、policy 名を取り違えても、あちらは緑のまま通る (#3369)。
func TestRolePolicyDrift(t *testing.T) {
	upstream := upstreamRolePolicies(upstreamMetas(t))
	// 拾えなくなったら落とす。件数の下限に加えて、名指しで要求する。
	if len(upstream) < 20 || upstream["invite/create"] != "canInvite" ||
		upstream["admin/emoji/add"] != "canManageCustomEmojis" || upstream["users/search"] != "canSearchUsers" {
		t.Fatalf("upstream requiredRolePolicy looks broken (%d endpoints, invite/create=%q, admin/emoji/add=%q, users/search=%q); regenerate with `make paramdef-gen`",
			len(upstream), upstream["invite/create"], upstream["admin/emoji/add"], upstream["users/search"])
	}

	regs := parseRouteRegistrations(t, serverSourcePaths(t)...)
	if len(regs) < 200 {
		t.Fatalf("parsed only %d routes from internal/server (expected >=200); the parser likely broke", len(regs))
	}

	var problems []string
	checked := 0
	// allowlist の項目のうち、この走査で扱ったもの (使った、または古いと報告した)。
	seenAllow := map[string]bool{}
	for ep, want := range upstream {
		if excludedEndpoint(ep) {
			continue
		}
		reg, ok := regs[ep]
		if !ok {
			continue // not implemented
		}
		got := routeRolePolicies(t, ep, reg)
		_, allowed := rolePolicyStricterThanUpstream[ep]
		switch {
		case got[want] && allowed:
			seenAllow[ep] = true
			problems = append(problems, ep+": applies "+want+" now; remove it from rolePolicyStricterThanUpstream")
		case got[want]:
		case allowed:
			seenAllow[ep] = true
		default:
			problems = append(problems, ep+": upstream requiredRolePolicy="+want+", Elythia RequireRolePolicy="+strings.Join(sortedKeys(got), ","))
		}
		checked++
	}
	// 検査できた endpoint が少なすぎれば落とす (route の読み取りが壊れたとき)。
	if checked < 20 {
		t.Fatalf("checked only %d endpoints with requiredRolePolicy (expected >=20); route parsing likely broke", checked)
	}
	// allowlist が実態より広いと、新しい欠落を隠す。本家に requiredRolePolicy が
	// 無いもの、Elythia が実装していないものも未使用として落とす。
	for ep := range rolePolicyStricterThanUpstream {
		if !seenAllow[ep] {
			problems = append(problems, ep+": unused entry of rolePolicyStricterThanUpstream (upstream has no requiredRolePolicy, or Elythia does not implement it)")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatal("role policy drift: Elythia's route does not apply upstream's requiredRolePolicy.\n" +
			"Fix the route, or register the stricter guard with its reason in rolePolicyStricterThanUpstream.\n  " +
			strings.Join(problems, "\n  "))
	}
}

// upstreamRolePolicies returns endpoint -> meta.requiredRolePolicy of every
// endpoint that declares one.
func upstreamRolePolicies(metas map[string]upstreamMeta) map[string]string {
	out := map[string]string{}
	for ep, m := range metas {
		if m.RequiredRolePolicy != nil {
			out[ep] = *m.RequiredRolePolicy
		}
	}
	return out
}

// routeRolePolicies returns the policy keys of every RequireRolePolicy in a
// route registration, resolving corerole.PolicyXxx constants.
func routeRolePolicies(t *testing.T, ep, reg string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, arg := range requireRolePolicyArgs(stripGoComments(reg)) {
		switch {
		case policyConstRe.MatchString(arg):
			out[policyConstValue(t, policyConstRe.FindStringSubmatch(arg)[1])] = true
		case policyLiteralRe.MatchString(arg):
			out[policyLiteralRe.FindStringSubmatch(arg)[1]] = true
		default:
			// 読めない形は落とす。変数を経由されると検査できない。
			t.Fatalf("%s: RequireRolePolicy の policy を定数かリテラルで書いていない: %s", ep, arg)
		}
	}
	return out
}
