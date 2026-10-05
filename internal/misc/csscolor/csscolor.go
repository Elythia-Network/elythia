// Package csscolor parses CSS color strings the way tinycolor2 does.
//
// upstream Misskey の `FetchInstanceMetadataService.getThemeColor` は
// `new tinycolor(themeColor)` で検証し、有効なら `toHexString()` (= `#rrggbb`)
// を保存、無効なら null にする。mk-go は長さと NUL しか見ておらず、`red` や
// `rgb(1,2,3)` をそのまま `instance.themeColor` に入れていた (#2726)。
//
// **tinycolor の挙動をそのまま写している。** 受理する形も、範囲外の値の丸め方も
// 独自判断を入れない。`rgb(300,0,0)` が `#ff0000` に、`rgb(-5,0,0)` が
// `#000000` に、`hsl(-60,...)` が (CSS の 300 度ではなく) `#ff0000` になるのは
// tinycolor がそう畳むからで、CSS の仕様ではない。
//
// `names.go` と `vectors_test.go` は**実物の tinycolor2 から生成したもの**で、
// 手で書き足さない。本家の tinycolor2 が上がったときは
// `internal/misc/csscolor/gen/gen.js` を実行して作り直す (写しているのは本家 backend の
// FetchInstanceMetadataService なので、本家の取得先の tinycolor2 を使う。
// `make upstream-e2e-deps` で pnpm install 済みになる):
//
//	cd .cache/misskey/<版>/packages/backend/node_modules/tinycolor2
//	node /path/to/mk/internal/misc/csscolor/gen/gen.js /path/to/mk
//
// 生成しなおしたら **`gofmt -w internal/misc/csscolor/` を掛けてから**
// `go test ./internal/misc/csscolor/` を回す (生成器は map の値を整列しないので、
// そのまま commit すると CI の `lint` が落ちる)。差分が出たら本 package を
// tinycolor の新しい挙動に合わせること。
package csscolor

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// tinycolor の matchers (tinycolor.js の `matchers`) をそのまま移したもの。
//
// **anchor しないのは関数形式 (rgb / hsl / hsv) だけ。** tinycolor も
// `new RegExp("rgb" + PERMISSIVE_MATCH3)` をそのまま exec するので、
// `xx rgb(1,2,3)` のような前後のゴミを許す。hex は下の `reHex*` のとおり
// `^#?...$` で anchor されていて、色名は `names` の完全一致なので、
// そちらは位置にも長さにも敏感 (`xxxred` は不正)。
const (
	cssNumber  = `[-\+]?\d*\.\d+%?`
	cssInteger = `[-\+]?\d+%?`
	cssUnit    = `(?:` + cssNumber + `)|(?:` + cssInteger + `)`

	// **`\s` を素で書かない。** Go の regexp の `\s` は `[\t\n\f\r ]` だけで、
	// `\v` も Unicode 空白も含まない。JS の `\s` (WhiteSpace + LineTerminator) は
	// それらを含むので、素で書くと `rgb(1,\u30002,3)` (全角スペース区切り) を
	// mk-go だけが reject する — upstream なら色が入るところが NULL になる
	// (#2726 のレビュー 2 周目で実測)。jsSpace と同じ集合を明示する。
	jsWS = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

	permissiveMatch3 = `[` + jsWS + `|\(]+(` + cssUnit + `)[,|` + jsWS + `]+(` + cssUnit +
		`)[,|` + jsWS + `]+(` + cssUnit + `)[` + jsWS + `]*\)?`
	permissiveMatch4 = `[` + jsWS + `|\(]+(` + cssUnit + `)[,|` + jsWS + `]+(` + cssUnit +
		`)[,|` + jsWS + `]+(` + cssUnit + `)[,|` + jsWS + `]+(` + cssUnit + `)[` + jsWS + `]*\)?`
)

var (
	reRGB  = regexp.MustCompile(`rgb` + permissiveMatch3)
	reRGBA = regexp.MustCompile(`rgba` + permissiveMatch4)
	reHSL  = regexp.MustCompile(`hsl` + permissiveMatch3)
	reHSLA = regexp.MustCompile(`hsla` + permissiveMatch4)
	reHSV  = regexp.MustCompile(`hsv` + permissiveMatch3)
	reHSVA = regexp.MustCompile(`hsva` + permissiveMatch4)

	reHex3 = regexp.MustCompile(`^#?([0-9a-fA-F]{1})([0-9a-fA-F]{1})([0-9a-fA-F]{1})$`)
	reHex4 = regexp.MustCompile(`^#?([0-9a-fA-F]{1})([0-9a-fA-F]{1})([0-9a-fA-F]{1})([0-9a-fA-F]{1})$`)
	reHex6 = regexp.MustCompile(`^#?([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`)
	reHex8 = regexp.MustCompile(`^#?([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`)
)

// Normalize returns the `#rrggbb` form of a CSS color string, mirroring
// tinycolor2's `c.isValid() ? c.toHexString() : null`.
//
// ok is false for anything tinycolor rejects. alpha は捨てる (`toHexString` も
// 捨てるので `rgba(0,0,0,0)` は `#000000`)。
func Normalize(s string) (hex string, ok bool) {
	r, g, b, ok := parse(s)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("#%02x%02x%02x", round255(r), round255(g), round255(b)), true
}

// parse mirrors tinycolor's stringInputToObject + inputToRGB for string input.
// 戻り値は 0-255 の実数 (丸めは呼び出し側)。
func parse(s string) (r, g, b float64, ok bool) {
	color := jsToLower(trimJSSpace(s))
	if hex, named := names[color]; named {
		color = hex
	} else if color == "transparent" {
		// tinycolor は `{r:0,g:0,b:0,a:0}` を返す。alpha は toHexString で
		// 落ちるので黒になる。
		return 0, 0, 0, true
	}

	if m := reRGB.FindStringSubmatch(color); m != nil {
		return bound01(m[1], 255) * 255, bound01(m[2], 255) * 255, bound01(m[3], 255) * 255, true
	}
	if m := reRGBA.FindStringSubmatch(color); m != nil {
		return bound01(m[1], 255) * 255, bound01(m[2], 255) * 255, bound01(m[3], 255) * 255, true
	}
	if m := reHSL.FindStringSubmatch(color); m != nil {
		r, g, b := hslToRGB(m[1], convertToPercentage(m[2]), convertToPercentage(m[3]))
		return r, g, b, true
	}
	if m := reHSLA.FindStringSubmatch(color); m != nil {
		r, g, b := hslToRGB(m[1], convertToPercentage(m[2]), convertToPercentage(m[3]))
		return r, g, b, true
	}
	if m := reHSV.FindStringSubmatch(color); m != nil {
		r, g, b := hsvToRGB(m[1], convertToPercentage(m[2]), convertToPercentage(m[3]))
		return r, g, b, true
	}
	if m := reHSVA.FindStringSubmatch(color); m != nil {
		r, g, b := hsvToRGB(m[1], convertToPercentage(m[2]), convertToPercentage(m[3]))
		return r, g, b, true
	}
	// hex は tinycolor と同じ hex8 → hex6 → hex4 → hex3 の順。**結果には効かない**
	// (4 本とも `^#?...$` で桁数が固定なので言語が互いに素) が、上流と読み比べる
	// ときに揃っているほうがよい。
	if m := reHex8.FindStringSubmatch(color); m != nil {
		return hexPair(m[1]), hexPair(m[2]), hexPair(m[3]), true
	}
	if m := reHex6.FindStringSubmatch(color); m != nil {
		return hexPair(m[1]), hexPair(m[2]), hexPair(m[3]), true
	}
	if m := reHex4.FindStringSubmatch(color); m != nil {
		return hexPair(m[1] + m[1]), hexPair(m[2] + m[2]), hexPair(m[3] + m[3]), true
	}
	if m := reHex3.FindStringSubmatch(color); m != nil {
		return hexPair(m[1] + m[1]), hexPair(m[2] + m[2]), hexPair(m[3] + m[3]), true
	}
	return 0, 0, 0, false
}

// hexPair parses a 2-digit hex component.
func hexPair(s string) float64 {
	v, _ := strconv.ParseInt(s, 16, 32)
	return float64(v)
}

// round255 clamps to [0,255] and rounds like JS `Math.round` (half up).
// 値は非負なので Go の math.Round (half away from zero) と一致する。
func round255(v float64) int {
	v = math.Min(255, math.Max(0, v))
	return int(math.Round(v))
}

// jsSpace reports whether r is whitespace for JS `\s` (= WhiteSpace +
// LineTerminator)。**上の jsWS と同じ集合**でなければならない
// (TestJSSpaceMatchesRegexpClass が突き合わせる)。
//
// **Go の `unicode.IsSpace` とは 2 つずれる。** JS は ZWNBSP (U+FEFF) を空白に
// 数えるが Go は数えず、Go は NEL (U+0085) を数えるが JS は数えない。tinycolor は
// 入力を `/^\s+/` / `/\s+$/` で trim するので、`strings.TrimSpace` を使うと
// `"\uFEFFred"` を落とし `"\u0085red"` を通してしまう (実測、#2726)。
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// trimJSSpace trims JS `\s` from both ends.
func trimJSSpace(s string) string {
	return strings.TrimFunc(s, jsSpace)
}

// jsToLower lowercases the way JS `String.prototype.toLowerCase()` does.
//
// **Go の `strings.ToLower` は simple case mapping** なので、full mapping を
// 使う JS と結果が分かれる符号位置がある。全 Unicode を突き合わせた実測では
// **ASCII に落ちる差は U+0130 (İ) だけ** — Go は `"i"`、JS は `"i" + U+0307`。
// この 1 文字のせいで `"İvory"` を mk-go だけが色名として受理していた
// (upstream は不正扱いで NULL を保存する)。
//
// 残る差は 2 系統で、どちらも合わせ込んでいない: Unicode 版差の 55 符号位置
// (`Ɤ` U+A7CB など。Go の table が上がれば消える) と、文脈依存の final sigma
// (`"ΑΣ"` → Go `ασ` / JS `ας`。符号位置ごとの比較では見えない)。**色名は
// ASCII、hex も ASCII、関数形式のキーワードも ASCII** なので、どちらも受理の
// 判定には届かない (Σ / ς を corpus に入れた差分 fuzz でも 0 件不一致、
// #2726 のレビュー 2 周目 / 5 周目で実測)。
func jsToLower(s string) string {
	if strings.ContainsRune(s, 0x0130) {
		s = strings.ReplaceAll(s, "\u0130", "i\u0307")
	}
	return strings.ToLower(s)
}

// parseFloatJS mirrors JS parseFloat: 先頭から読める分だけ読む ("50%" → 50)。
// 数字が 1 つも無ければ NaN。
//
// **桁溢れは NaN にしない。** CSS_INTEGER は桁数を縛らないので `rgb(999...9,0,0)`
// (400 桁) が実際に届きうる。JS の parseFloat は Infinity を返し、tinycolor は
// それを 255 に clamp して `#ff0000` にする (実測)。ここで NaN にすると以降の
// 演算がすべて NaN になり、丸めの結果が未定義になる。
//
// **指数部も読む。** CSS unit の正規表現は `e` を通さないが、
// `convertToPercentage` が作る文字列が bound01 へ渡る。`.000000003` は
// `String(3e-9 * 100) + "%"` = `"3e-7%"` になるので、そこで `3` と読むと
// 結果がずれる (#2726)。
func parseFloatJS(s string) float64 {
	end := 0
	seenDigit := false
	seenDot := false
	i := 0
	for ; i < len(s); i++ {
		c := s[i]
		if i == 0 && (c == '+' || c == '-') {
			end = i + 1
			continue
		}
		if c >= '0' && c <= '9' {
			seenDigit = true
			end = i + 1
			continue
		}
		if c == '.' && !seenDot {
			seenDot = true
			end = i + 1
			continue
		}
		break
	}
	if !seenDigit {
		return math.NaN()
	}
	// 指数部は「`e` の直後に (符号+) 数字が 1 つ以上」あるときだけ取り込む。
	// 揃わなければ小数部までで打ち切る (JS の parseFloat と同じ)。
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		k := j
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k > j {
			end = k
		}
	}
	// err は範囲外 (= ±Inf) でのみ返る。値はそのまま使う (上のコメント参照)。
	v, _ := strconv.ParseFloat(s[:end], 64)
	return v
}

// parseIntJS mirrors JS `parseInt(s, 10)`: 空白を飛ばし、符号と 10 進数字だけを
// 読む。数字が無ければ NaN。
//
// **`e` で止まるのが要点。** `parseInt(1e-7)` が 1 になるのは、JS が数値を先に
// 文字列化して `"1e-7"` の先頭 1 桁だけを読むため。`math.Trunc` で代用すると 0 に
// なってずれる (#2726)。
func parseIntJS(s string) float64 {
	s = trimJSSpace(s)
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return math.NaN()
	}
	v, _ := strconv.ParseFloat(s[start:i], 64)
	if neg {
		return -v
	}
	return v
}

// jsNumberToString mirrors ECMAScript の Number::toString (radix 10) for finite
// values.
//
// **`strconv.FormatFloat(v, 'g', -1, 64)` では代用できない。** Go の `g` は
// 指数表記へ切り替わる境界が 1e-4 付近だが、JS は 1e-6 まで固定小数のままで、
// 上は 1e21 で切り替わる。`convertToPercentage` の戻り値がこの形で
// `bound01` へ渡るので、境界がずれると小さい s / l / v を持つ hsl / hsv の
// 結果が変わる (#2726)。
func jsNumberToString(v float64) string {
	if v == 0 {
		return "0"
	}
	if v < 0 {
		return "-" + jsNumberToString(-v)
	}
	if math.IsInf(v, 1) {
		return "Infinity"
	}
	if math.IsNaN(v) {
		return "NaN"
	}
	// 最短往復表現から仮数の桁列 (digits) と 10 の指数 (exp) を取り出す。
	e := strconv.FormatFloat(v, 'e', -1, 64) // "d.ddde±dd"
	mant, expPart, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expPart)
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits)
	n := exp + 1 // 小数点の位置 (spec の n)

	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	}
	// 指数表記。JS は指数の符号を必ず出す。
	sign := "+"
	ex := n - 1
	if ex < 0 {
		sign = "-"
		ex = -ex
	}
	if k == 1 {
		return digits + "e" + sign + strconv.Itoa(ex)
	}
	return digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(ex)
}

// bound01 mirrors tinycolor's bound01: 入力を 0..1 に畳む。
//
// **独自に整理しない。** 順序と早期 return がそのまま出力に効く:
//   - 先に `[0, max]` へ clamp するので、負の hue は 0 (= 赤) になる。
//     `hsl(-60,...)` が CSS の 300 度ではなく 0 度になるのはこのため
//   - `Math.abs(n - max) < 0.000001` の早期 return が無いと、`rgb(255,..)` が
//     `255 % 255 = 0` で黒に落ちる
func bound01(n string, max float64) float64 {
	value := n
	// isOnePointZero: "1.0" のような **小数点を含む** 1 は 100% 扱い。
	if strings.Contains(n, ".") && parseFloatJS(n) == 1 {
		value = "100%"
	}
	processPercent := strings.Contains(value, "%")

	v := math.Min(max, math.Max(0, parseFloatJS(value)))
	if processPercent {
		// JS の `parseInt(n * max, 10)` は**数値を文字列にしてから**読むので、
		// `math.Trunc` とは小さい値で結果が分かれる (`parseInt(1e-7) === 1`)。
		v = parseIntJS(jsNumberToString(v*max)) / 100
	}
	if math.Abs(v-max) < 0.000001 {
		return 1
	}
	return math.Mod(v, max) / max
}

// convertToPercentage mirrors tinycolor's convertToPercentage: 1 以下の値は
// 割合表記とみなして "N%" に直す。
//
// JS は文字列を数値へ暗黙変換して比較するので、"100%" のような数値化できない
// 文字列は NaN <= 1 = false になり、そのまま返る。
func convertToPercentage(n string) string {
	v, err := strconv.ParseFloat(trimJSSpace(n), 64)
	if err != nil || math.IsNaN(v) {
		return n
	}
	if v <= 1 {
		return jsNumberToString(v*100) + "%"
	}
	return n
}

// hslToRGB mirrors tinycolor's hslToRgb. 戻り値は 0-255 の実数。
func hslToRGB(hRaw, sRaw, lRaw string) (float64, float64, float64) {
	h := bound01(hRaw, 360)
	s := bound01(sRaw, 100)
	l := bound01(lRaw, 100)

	if s == 0 {
		return l * 255, l * 255, l * 255
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	return hue2rgb(p, q, h+1.0/3.0) * 255, hue2rgb(p, q, h) * 255, hue2rgb(p, q, h-1.0/3.0) * 255
}

func hue2rgb(p, q, t float64) float64 {
	if t < 0 {
		t++
	}
	if t > 1 {
		t--
	}
	switch {
	case t < 1.0/6.0:
		return p + (q-p)*6*t
	case t < 1.0/2.0:
		return q
	case t < 2.0/3.0:
		return p + (q-p)*(2.0/3.0-t)*6
	default:
		return p
	}
}

// hsvToRGB mirrors tinycolor's hsvToRgb. 戻り値は 0-255 の実数。
func hsvToRGB(hRaw, sRaw, vRaw string) (float64, float64, float64) {
	h := bound01(hRaw, 360) * 6
	s := bound01(sRaw, 100)
	v := bound01(vRaw, 100)

	i := math.Floor(h)
	f := h - i
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	// bound01 が [0,1] を返すので h は [0,6]、i は [0,6] に収まる。
	// `i % 6` は負にならない。
	mod := int(math.Mod(i, 6))
	r := [6]float64{v, q, p, p, t, v}[mod]
	g := [6]float64{t, v, v, q, p, p}[mod]
	b := [6]float64{p, p, t, v, v, q}[mod]
	return r * 255, g * 255, b * 255
}

// Names returns every colour name tinycolor accepts. テスト用。
func Names() []string {
	out := make([]string, 0, len(names))
	for k := range names {
		out = append(out, k)
	}
	return out
}
