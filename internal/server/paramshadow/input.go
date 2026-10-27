// Package paramshadow runs upstream's paramDef validation (internal/api/
// paramschema) in front of the API handlers. For most upstream endpoints it
// does not act on it and counts the requests where its verdict and the
// handler's answer disagree (#3677); for the endpoints aligned with upstream
// (WithEnforced, #3742) it answers a rejected request with upstream's 400.
//
// The middleware never changes what the handler reads: it reads the JSON
// body (and puts the same bytes back), decodes its own copy and validates
// that copy. What it records is the endpoint name, the
// schemaPath of the validator's first error, the INVALID_PARAM info.param of
// the handler's answer, the handler's status and error code, and the kind of
// disagreement. It never records a value, the body, the user, the IP or the
// token.
package paramshadow

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// Skip reasons: why a request to an upstream endpoint was not validated.
const (
	// SkipNotJSON: a POST whose body is not application/json. Upstream's
	// answer to such a body comes from fastify (no body, text/plain, an
	// unsupported type) and requireObjectBody already mirrors it.
	SkipNotJSON = "not_json"
	// SkipMultipart: a requireFile endpoint (drive/files/create). Upstream
	// takes the params from the multipart fields @fastify/multipart parsed
	// before the file part, and a repeated field becomes undefined. Go's
	// parsed form has lost that order, so the input cannot be rebuilt.
	SkipMultipart = "multipart"
	// SkipMethod: a GET to an endpoint without meta.allowGet (upstream
	// answers it before the params), or a method other than GET and POST.
	SkipMethod = "method"
	// SkipLoneSurrogate: the JSON body escapes a surrogate that is not half
	// of a pair ("\ud800"). JSON.parse keeps it, encoding/json turns it into
	// U+FFFD, so enum and uniqueItems could judge differently.
	SkipLoneSurrogate = "lone_surrogate"
	// SkipInvalidUTF8: the body or the query is not valid UTF-8. Node and Go
	// replace the bad bytes with a different number of U+FFFD, which changes
	// the length ajv counts.
	SkipInvalidUTF8 = "invalid_utf8"
	// SkipQuery: a query url.ParseQuery rejects (a ';' or a bad
	// %-escape). fast-querystring parses it differently.
	SkipQuery = "unparsable_query"
	// SkipTooLarge: the JSON body or the query is longer than the limit
	// (DefaultMaxBody). Decoding a body into Go values allocates many times
	// its size, so a body near the 1MiB API limit is not validated.
	SkipTooLarge = "too_large"
	// SkipTooManyValues: the JSON body has more tokens (keys, values and
	// brackets) than the limit (DefaultMaxValues), or the query has more
	// params. Decoding allocates per value, so a body of many tiny values
	// ({"x":[{},{},…]}) costs the most per byte.
	SkipTooManyValues = "too_many_values"
	// SkipBusy: as many validations as the limit (DefaultMaxConcurrent) were
	// running. The request is not validated rather than made to wait.
	SkipBusy = "busy"
	// SkipBudget: the validation time of the recent past used up the budget
	// (DefaultBudgetCores, DefaultBudgetBurst). The request is not validated
	// until enough of the budget has come back.
	SkipBudget = "budget"
	// SkipBody: reading or decoding the body failed. JSONBodyParse has
	// validated it, so this is not expected.
	SkipBody = "body_error"
)

// DefaultMaxBody is the largest JSON body (and query) validated, in bytes.
//
// 本家のクライアントが普通に送るbodyは、これより十分に小さい(#3677で、
// 上限まで埋めたbodyをJSONにして数えた)。notes/createは、本文3000文字を
// すべて4バイトの絵文字にし、cw・投票10択・ファイル16個・宛先20人を付けて
// 14,750バイト。i/updateは、名前・自己紹介1500文字を絵文字で、項目16組を
// 上限まで埋めて22,950バイト。届かないのは、管理者がadmin/update-metaで
// 大きな一覧を送る場合で(blockedHostsは1000件で27,057バイト、3000件で
// 81,018バイト)、too_largeとして数える。
const DefaultMaxBody = 64 << 10

// DefaultMaxValues is the largest number of JSON tokens (each key, scalar
// value and bracket counts as one) or query params validated.
//
// 64KiBに収まる正当なbodyでも、トークンの数は数千まで。#3677で数えた値は、
// admin/update-metaのblockedHosts 2000件(54,057バイト)が2,007、i/updateの
// ワードミュートとハードミュートを200件ずつが1,210、notes/createを上限まで
// 埋めたものが78。64KiBいっぱいの`{}`の配列は約4.4万になり、decodeの確保の
// 大半はここから来る(#3677のレビュー2周目M-1)。
const DefaultMaxValues = 4096

// DefaultEnforcedMaxBody is the size limit of the JSON body (and query) of
// the endpoints that reject (WithEnforced), in bytes. A larger one goes to
// the handler unvalidated, like SkipTooLarge.
//
// 1件の費用の上限を、トークンの数だけでなく大きさでも抑える。64KiBの`\u`の
// escapeの文字列は、検証ありで1件0.52ms・311KBかかった。16KiBでは約0.14ms・
// 83KB(docs/param-shadow.md)。clips/*の正当なbodyは、UTF-8のまま送れば
// descriptionの2048文字を4バイトの文字で埋めても約8KiB。charts/*と
// hashtags/*はもっと小さい(項目は最大6つで、長さに上限の無い文字列は
// charts/*のuserIdとhost、hashtags/*のqueryとtagだけ)。
const DefaultEnforcedMaxBody = 16 << 10

// DefaultEnforcedMaxValues is the token limit of the endpoints that reject
// (WithEnforced). A request over it goes to the handler unvalidated, like
// SkipTooManyValues.
//
// 拒否するendpointは予算と枠を通さないので、1件の費用をトークンの数で抑える
// (#3742のレビュー1周目M-1)。正当なbodyは多くて十数トークン(clips/notesの
// 全項目と`i`で18、charts/*で12、hashtags/usersで16)。本家のparamDefの項目の
// 数に対して十分に大きく、decodeの確保は上限の4096より1桁以上小さくなる。
// 例外はantennas/create・updateで、本家はkeywords・excludeKeywords・usersの
// 数に上限を持たないので、多く並べたbodyは超えうる。超えたbodyは拒否せず
// handlerへ渡す(#3748)。flash/create・updateも、scriptが約16KiBを超えると
// 大きさの上限を超える(#3750)。gallery/posts/create・updateのdescriptionも
// 本家に上限が無い(#3754)。channels/updateのpinnedNoteIdsも本家に上限が無く、
// 同梱のfrontendが`i`と一緒に送るbodyでは236件からトークンの上限を超える(#3758)。
// pages/create・updateも、本家がcontentの数にも文字列の長さにも上限を持たず、
// 同梱のエディタのbodyではtextのブロック29個からトークンの上限を超える(#3772)。
// users/showのuserIdsとusers/relationのuserIdの配列も本家に数の上限が無く、
// `i`と一緒に送るbodyでは250件からトークンの上限を超える(#3837)。
// admin/roles/create・updateも、本家がpoliciesの中身を決めず、同梱のロールの
// エディタは全部のポリシーを埋めて送るので、普段の保存で超える(updateは
// ポリシー24件から、roleIdの無いcreateは25件から。#3859)。
const DefaultEnforcedMaxValues = 256

// input is what upstream hands to ajv for one request, or why the request is
// not validated.
type input struct {
	data any
	// castFailed is the param upstream's cast step (GET) fails on before ajv
	// runs: it answers INVALID_PARAM {param: castFailed} instead.
	castFailed string
	skip       string
}

// readBody reads the request body when it is at most max bytes and puts the
// same bytes back, so the handler reads exactly what it would have read. A
// body whose Content-Length is over max is not read at all; a longer body of
// unknown length is read only up to max+1 bytes. Both are reported as
// tooLarge, and the handler reads the whole body as it was. When the read
// fails, the handler gets the bytes read so far followed by the same error.
func readBody(req *http.Request, max int) (data []byte, tooLarge bool, err error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, false, nil
	}
	// Content-Lengthが上限を超えていれば、読まずにtoo_largeにする(#3677の
	// レビュー3周目L-1)。読んで写すと、1MiBのbodyを送り続けられたときに、
	// 1件ごとに上限+1バイトの確保が残る。bodyに触らないので、handlerの読むものは
	// そのまま。JSONBodyParseが剥がしたBOMの3バイトはContent-Lengthに残るので、
	// BOM付きで剥がすと上限に収まるbodyもtoo_largeになる(数え方だけの差)。
	if req.ContentLength > int64(max) {
		return nil, true, nil
	}
	rest := req.Body
	// 確保は1度で済ませる。io.ReadAllは512バイトから倍々に伸ばすので、上限近くの
	// bodyでは本文の約3倍を確保していた。Content-Lengthが分かれば(ここでは上限
	// 以下)その大きさ、分からなければ(chunked、Content-Lengthの無いHTTP/2)上限+1。
	// bytes.MinReadの余白は、読み終えた後のEOFを読むためにReadFromが要求する分。
	size := max + 1
	if req.ContentLength >= 0 {
		size = int(req.ContentLength)
	}
	buf := bytes.NewBuffer(make([]byte, 0, size+bytes.MinRead))
	// 上限+1バイトまでしか読まない。1MiBに近いbodyを写してdecodeすると、
	// 確保するメモリが本文の数十倍になる(#3677のレビューH-1)。
	_, err = buf.ReadFrom(io.LimitReader(rest, int64(max)+1))
	data = buf.Bytes()
	if err != nil {
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), errReader{err}))
		return nil, false, err
	}
	if len(data) > max {
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), rest))
		return nil, true, nil
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	return data, false, nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// jsonInput decodes a JSON body (already checked by JSONBodyParse) the way
// upstream's ajv sees it: numbers stay json.Number, so 1e400 is Infinity
// rather than an error and 10.0 is still an integer.
func jsonInput(body []byte) input {
	if !utf8.Valid(body) {
		return input{skip: SkipInvalidUTF8}
	}
	if hasLoneSurrogate(body) {
		return input{skip: SkipLoneSurrogate}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return input{skip: SkipBody}
	}
	return input{data: v}
}

// tooManyValues reports whether body, valid JSON, has more than max tokens:
// the tokens json.Decoder.Token returns (each key, scalar value and bracket).
// It scans the bytes without allocating and stops at the first token over
// max.
//
// **json.Decoder.Tokenでは数えない。** 数は同じだが、Tokenは文字列と数ごとに
// 確保し、Decoderは自分のbufferへbodyを写し直す。上限ちょうどの最悪のbodyでは、
// 数えるだけで検証を飛ばしたぶんの多くを使ってしまった(#3677のレビュー2周目
// M-1で測った。値はdocs/param-shadow.md)。数がTokenと一致することは
// TestTooManyValuesMatchesDecoderTokenで照合する。
func tooManyValues(body []byte, max int) bool {
	n := 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '{' || c == '}' || c == '[' || c == ']':
		case c == '"':
			// 文字列は閉じる"まで飛ばす。\の次の文字は、"でも閉じない。
			for i++; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' {
					i++
				}
			}
		case c == '-' || c >= '0' && c <= '9' || c == 't' || c == 'f' || c == 'n':
			// 数とtrue / false / nullは、区切りの手前まで1つ。
			for i+1 < len(body) && !isJSONDelimiter(body[i+1]) {
				i++
			}
		default:
			// , : と空白はトークンではない。
			continue
		}
		n++
		if n > max {
			return true
		}
	}
	return false
}

func isJSONDelimiter(c byte) bool {
	switch c {
	case ',', ':', ']', '}', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// queryTooManyValues reports whether rawQuery has more than max params.
func queryTooManyValues(rawQuery string, max int) bool {
	return rawQuery != "" && strings.Count(rawQuery, "&")+1 > max
}

// hasLoneSurrogate reports whether body, valid JSON, has a \u escape of a
// surrogate that is not half of a high-low pair.
func hasLoneSurrogate(body []byte) bool {
	// 正しいJSONでは、`\`は文字列の中にしか現れない。`\`の無いbodyは走査しない。
	if bytes.IndexByte(body, '\\') < 0 {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		if i+1 >= len(body) || body[i+1] != 'u' {
			// `\\`や`\"`は2文字で1つ。次の文字を飛ばす。
			i++
			continue
		}
		r, ok := hex4(body, i+2)
		if !ok {
			i++
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			if i+7 < len(body) && body[i+6] == '\\' && body[i+7] == 'u' {
				if lo, ok := hex4(body, i+8); ok && lo >= 0xDC00 && lo <= 0xDFFF {
					i += 11
					continue
				}
			}
			return true
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		}
		i += 5
	}
	return false
}

func hex4(b []byte, at int) (rune, bool) {
	if at+4 > len(b) {
		return 0, false
	}
	var r rune
	for _, c := range b[at : at+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}

// queryInput builds the params upstream validates for a GET: the query
// (fastify's fast-querystring, where a repeated key becomes an array of its
// values) after the cast step, which JSON.parses the string value of every
// boolean / number / integer property and fails on the first value that does
// not parse (ApiCallService.ts call()).
func queryInput(rawQuery string, s *paramschema.Schema) input {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return input{skip: SkipQuery}
	}
	data := make(map[string]any, len(q))
	for k, vs := range q {
		if !utf8.ValidString(k) {
			return input{skip: SkipInvalidUTF8}
		}
		for _, v := range vs {
			if !utf8.ValidString(v) {
				return input{skip: SkipInvalidUTF8}
			}
		}
		if len(vs) == 1 {
			data[k] = vs[0]
			continue
		}
		arr := make([]any, len(vs))
		for i, v := range vs {
			arr[i] = v
		}
		data[k] = arr
	}
	for _, p := range s.ScalarParams() {
		str, ok := data[p.Name].(string)
		if !ok {
			continue
		}
		// JSON.parseが受け付けるのは、前後の空白を除いて1つの値だけの文字列。
		// json.Validも同じ(UTF-8の正しさは上で見た)。
		if !json.Valid([]byte(str)) {
			return input{castFailed: p.Name}
		}
		dec := json.NewDecoder(strings.NewReader(str))
		dec.UseNumber()
		var v any
		_ = dec.Decode(&v)
		data[p.Name] = v
	}
	return input{data: data}
}
