package activitypub

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// referenceSigKVPattern is the regular expression ParseSignatureHeader used
// before #4265. The scanner must return exactly what it matched.
var referenceSigKVPattern = regexp.MustCompile(`(\w+)="([^"]+)"`)

// referenceParseSignatureHeader is the regular-expression implementation of
// ParseSignatureHeader before #4265, kept verbatim as the oracle.
func referenceParseSignatureHeader(header string) (*ParsedSignature, error) {
	if header == "" {
		return nil, errors.New("missing signature header")
	}
	out := &ParsedSignature{}
	for _, m := range referenceSigKVPattern.FindAllStringSubmatch(header, -1) {
		switch m[1] {
		case "keyId":
			out.KeyID = m[2]
		case "algorithm":
			out.Algorithm = m[2]
		case "headers":
			out.Headers = strings.Fields(m[2])
		case "signature":
			out.Signature = m[2]
		}
	}
	if out.KeyID == "" || out.Signature == "" {
		return nil, errors.New("invalid signature header: keyId and signature are required")
	}
	if len(out.Headers) == 0 {
		out.Headers = []string{"date"}
	}
	return out, nil
}

// referenceSignaturePairs returns the (name, value) pairs the old regular
// expression found, in order.
func referenceSignaturePairs(header string) [][2]string {
	var out [][2]string
	for _, m := range referenceSigKVPattern.FindAllStringSubmatch(header, -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

// scannedSignaturePairs returns the (name, value) pairs the scanner finds, in
// order.
func scannedSignaturePairs(header string) [][2]string {
	var out [][2]string
	for rest := header; ; {
		name, value, next, ok := nextSignatureParam(rest)
		if !ok {
			return out
		}
		out = append(out, [2]string{name, value})
		rest = next
	}
}

// assertSameAsReference fails the test when ParseSignatureHeader and the
// regular-expression oracle disagree on in, either in the pairs found or in
// the parsed result.
func assertSameAsReference(t testing.TB, in string) {
	t.Helper()
	if got, want := scannedSignaturePairs(in), referenceSignaturePairs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs of %q:\n got  %q\n want %q", in, got, want)
	}
	got, gotErr := ParseSignatureHeader(in)
	want, wantErr := referenceParseSignatureHeader(in)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("error of %q: got %v, want %v", in, gotErr, wantErr)
	}
	if gotErr != nil {
		if gotErr.Error() != wantErr.Error() {
			t.Fatalf("error of %q: got %q, want %q", in, gotErr, wantErr)
		}
		if got != nil {
			t.Fatalf("error %v of %q with a result %+v", gotErr, in, got)
		}
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result of %q:\n got  %+v\n want %+v", in, got, want)
	}
}

func TestParseSignatureHeader_MatchesRegexp(t *testing.T) {
	sig344 := strings.Repeat("A", 342) + "=="
	cases := []struct {
		name string
		in   string
	}{
		{"misskey shape", `keyId="https://remote.example/users/9abc#main-key",algorithm="rsa-sha256",headers="(request-target) date host digest",signature="` + sig344 + `"`},
		{"mastodon shape with spaces", `keyId="https://remote.example/users/a#main-key", algorithm="rsa-sha256", headers="(request-target) host date digest content-type", signature="c2ln"`},
		{"minimal", `keyId="k",signature="s"`},
		{"empty", ``},
		{"only spaces", `   `},
		{"empty headers value is not a pair", `keyId="k",headers="",signature="s"`},
		{"whitespace headers default to date", `keyId="k",headers="   ",signature="s"`},
		{"later whitespace headers override earlier", `keyId="k",headers="host",headers=" ",signature="s"`},
		{"padded headers", `keyId="k", headers="  date   host ", signature="s"`},
		{"tab and newline in headers", "keyId=\"k\",headers=\"date\thost\ndigest\",signature=\"s\""},
		{"later empty key id does not clear", `keyId="k",signature="s",keyId=""`},
		{"later empty headers does not reset", `keyId="k",headers="host",headers="",signature="s"`},
		{"later empty algorithm does not clear", `keyId="k",algorithm="rsa-sha256",algorithm="",signature="s"`},
		{"empty signature", `keyId="k",signature=""`},
		{"empty key id", `keyId="",signature="s"`},
		{"unquoted values", `keyId=k,signature=s`},
		{"no separator between pairs", `keyId="a"keyId="b",signature="s"`},
		{"duplicate key id last wins", `keyId="a",keyId="b",signature="s"`},
		{"duplicate signature last wins", `keyId="a",signature="s1",signature="s2"`},
		{"duplicate algorithm last wins", `keyId="a",algorithm="rsa-sha256",algorithm="hs2019",signature="s"`},
		{"hs2019 with created and expires", `keyId="k",algorithm="hs2019",created=1700000000,expires=1700000300,headers="(created) (expires) host",signature="s"`},
		{"quoted created and expires are ignored", `keyId="k",created="1",expires="2",signature="s"`},
		{"leading scheme token", `Signature keyId="k",signature="s"`},
		{"leading hs2019 token", `hs2019 keyId="k",created=1,expires=2,signature="s"`},
		{"upper case names are not known", `KEYID="k",Signature="s"`},
		{"mixed case keyid is not known", `keyid="k",signature="s"`},
		{"prefixed name", `xkeyId="k",signature="s"`},
		{"suffixed name", `keyIdx="k",signature="s"`},
		{"later prefixed names do not override", `keyId="k",signature="s",_keyId="x",xsignature="y",signature1="z"`},
		{"underscore and digits in name", `_k_1="v",keyId="k",signature="s"`},
		{"name byte 0 before a known name", `keyId="k",signature="s",0keyId="x"`},
		{"name byte 0 inside a name", `keyId="k",signature="s",k0eyId="x"`},
		{"name byte 9 before a known name", `keyId="k",signature="s",9keyId="x"`},
		{"name byte 9 inside a name", `keyId="k",signature="s",k9eyId="x"`},
		{"name byte A before a known name", `keyId="k",signature="s",AkeyId="x"`},
		{"name byte A inside a name", `keyId="k",signature="s",kAeyId="x"`},
		{"name byte Z before a known name", `keyId="k",signature="s",ZkeyId="x"`},
		{"name byte Z inside a name", `keyId="k",signature="s",kZeyId="x"`},
		{"name byte a before a known name", `keyId="k",signature="s",akeyId="x"`},
		{"name byte a inside a name", `keyId="k",signature="s",kaeyId="x"`},
		{"name byte z before a known name", `keyId="k",signature="s",zkeyId="x"`},
		{"name byte z inside a name", `keyId="k",signature="s",kzeyId="x"`},
		{"name byte _ before a known name", `keyId="k",signature="s",_keyId="x"`},
		{"name byte _ inside a name", `keyId="k",signature="s",k_eyId="x"`},
		{"non name byte / before a known name", `keyId="k",signature="s",/keyId="x"`},
		{"non name byte / inside a name", `keyId="k",signature="s",k/eyId="x"`},
		{"non name byte : before a known name", `keyId="k",signature="s",:keyId="x"`},
		{"non name byte : inside a name", `keyId="k",signature="s",k:eyId="x"`},
		{"non name byte @ before a known name", `keyId="k",signature="s",@keyId="x"`},
		{"non name byte @ inside a name", `keyId="k",signature="s",k@eyId="x"`},
		{"non name byte [ before a known name", `keyId="k",signature="s",[keyId="x"`},
		{"non name byte [ inside a name", `keyId="k",signature="s",k[eyId="x"`},
		{"non name byte backquote before a known name", "keyId=\"k\",signature=\"s\",`keyId=\"x\""},
		{"non name byte backquote inside a name", "keyId=\"k\",signature=\"s\",k`eyId=\"x\""},
		{"non name byte { before a known name", `keyId="k",signature="s",{keyId="x"`},
		{"non name byte { inside a name", `keyId="k",signature="s",k{eyId="x"`},
		{"space before equals", `keyId ="k",signature="s"`},
		{"space after equals", `keyId= "k",signature="s"`},
		{"single quotes", `keyId='k',signature='s'`},
		{"unterminated value", `keyId="k",signature="s`},
		{"unterminated key id swallows the rest", `keyId="k,signature="s"`},
		{"backslash escape is not special", `keyId="a\"b",signature="s"`},
		{"backslash at end of value", `keyId="a\",signature="s"`},
		{"equals and commas inside value", `keyId="a=b,c=\"d",signature="s"`},
		{"value that looks like a pair", `x="keyId="k",signature="s"`},
		{"double quote run", `keyId=""k"",signature="s"`},
		{"equals equals", `keyId=="k",signature="s"`},
		{"name ending at quote", `a"keyId="k",signature="s"`},
		{"nul bytes", "keyId=\"k\x00\",signature=\"s\x00s\""},
		{"nul in name", "key\x00Id=\"k\",signature=\"s\""},
		{"non ascii name", `kéyId="k",keyId="k2",signature="s"`},
		{"non ascii value", `keyId="https://remote.example/é#k",signature="s"`},
		{"invalid utf8 in value", "keyId=\"\xff\xfe\",signature=\"s\""},
		{"invalid utf8 before name", "\xffkeyId=\"k\",signature=\"s\""},
		{"utf8 continuation byte then name", "\xc3keyId=\"k\",signature=\"s\""},
		{"newline inside value", "keyId=\"a\nb\",signature=\"s\""},
		{"crlf between pairs", "keyId=\"k\",\r\n signature=\"s\""},
		{"trailing garbage", `keyId="k",signature="s",,,=="`},
		{"lone equals quote", `="v",keyId="k",signature="s"`},
		{"name at end", `keyId="k",signature="s",abc`},
		{"name equals at end", `keyId="k",signature="s",abc=`},
		{"name equals quote at end", `keyId="k",signature="s",abc="`},
		{"only one quote", `"`},
		{"only equals quote quote", `a=""`},
		{"value single char", `keyId="k",signature="="`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSameAsReference(t, tc.in)
		})
	}
}

// TestParseSignatureHeader_MatchesRegexpRandom compares the scanner with the
// regular expression on random strings built from the characters that matter
// to the grammar, plus white space, NUL and non-ASCII bytes.
func TestParseSignatureHeader_MatchesRegexpRandom(t *testing.T) {
	// 文法に効く文字(語の文字・=・"・区切り)を多めにし、既知の名前も部品として混ぜる。
	// 既知の名前が出ないと、結果の比較が「どちらもエラー」ばかりになるため
	atoms := []string{
		"a", "b", "_", "1", "Z", "=", `"`, `"`, "=\"", ",", " ", "\t", "\n", "\\", "-", "\x00", "é", "\xff", "\xc3",
		// 名前の文字の範囲([0-9A-Za-z_])の端と、そのすぐ外のASCII
		"0", "9", "A", "z", "/", ":", "@", "[", "`", "{",
		"keyId", "signature", "algorithm", "headers", "created", "expires", "keyid", "KeyId",
		`keyId="k"`, `signature="s"`, `headers="date host"`, `headers=" "`, `algorithm="rsa-sha256"`,
		`keyId="k2"`, `signature="s2"`, `headers="host"`, `algorithm="hs2019"`,
	}
	r := rand.New(rand.NewPCG(4265, 1))
	// -race付きのCIでも数秒で済む件数にする。より深く探すのはFuzzParseSignatureHeader
	const n = 30000
	accepted := 0
	for range n {
		var b strings.Builder
		for range r.IntN(16) {
			b.WriteString(atoms[r.IntN(len(atoms))])
		}
		in := b.String()
		assertSameAsReference(t, in)
		if _, err := referenceParseSignatureHeader(in); err == nil {
			accepted++
		}
	}
	t.Logf("%d of %d random inputs were accepted", accepted, n)
	// 受け付ける入力がほとんど無いと、受け付けた結果の比較が空振りする
	if accepted < n/100 {
		t.Fatalf("only %d of %d random inputs were accepted; the comparison is too weak", accepted, n)
	}
}

// TestParseSignatureHeader_LinearTime checks that one call stays linear in the
// length of the header on inputs that make a naive scanner read the rest of
// the header again for every name.
func TestParseSignatureHeader_LinearTime(t *testing.T) {
	// 受付では署名を確かめる前に誰の要求でも解析する。1MBなら線形で数ms(-race付きでも
	// 数十ms)。名前ごとに見出しを1文字ずつ読み直す2乗の形は数分かかるので、withinの
	// 5秒で揺れずに分かれる。IndexByteで読み直す2乗の形は速い機械では10秒前後で、
	// 上限との差は数倍しかない
	const size = 1 << 20
	cases := map[string]string{
		"one long name":              strings.Repeat("a", size),
		"long names without quotes":  strings.Repeat("ab=", size/3),
		"names before one quote":     strings.Repeat("ab=", size/3) + `"`,
		"repeated name equals quote": strings.Repeat(`a="`, size/3),
		"empty values":               strings.Repeat(`a=""`, size/4),
		"equals quote only":          strings.Repeat(`="`, size/2),
		"one open value":             `a="` + strings.Repeat("b", size),
		"empty and unterminated mix": strings.Repeat(`a=""b="`, size/7),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			within(t, func() { _, _ = ParseSignatureHeader(in) })
		})
	}
}

// misskeySignatureHeader is a Signature header in the shape Misskey sends:
// keyId, algorithm, headers and a 344-character RSA-2048 signature.
var misskeySignatureHeader = `keyId="https://remote.example/users/9xyzabcdef#main-key",algorithm="rsa-sha256",headers="(request-target) date host digest",signature="` + strings.Repeat("Q", 342) + `=="`

func BenchmarkParseSignatureHeader(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseSignatureHeader(misskeySignatureHeader); err != nil {
			b.Fatal(err)
		}
	}
}
