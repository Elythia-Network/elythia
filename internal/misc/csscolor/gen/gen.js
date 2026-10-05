// names.go と vectors_test.go を実物の tinycolor2 から作り直す。
//
// 使い方 (tinycolor2 の package ディレクトリで実行する):
//
//   cd .cache/misskey/<版>/packages/backend/node_modules/tinycolor2
//   node <repo>/internal/misc/csscolor/gen/gen.js <repo>
//
// `Math.random` は使わない (生成物を commit するので、同じ入力から同じ
// vector が出ること自体が差分レビューの前提になる)。
'use strict';

const fs = require('fs');
const path = require('path');

// Go の文字列リテラルとして安全に出す。**JSON.stringify は使えない** — U+FEFF を
// そのまま埋めると Go が「illegal byte order mark」で弾く。
function goString(s) {
	let out = '"';
	for (const ch of s) {
		const cp = ch.codePointAt(0);
		if (ch === '"') out += '\\"';
		else if (ch === '\\') out += '\\\\';
		else if (cp >= 0x20 && cp < 0x7f) out += ch;
		else if (cp <= 0xffff) out += '\\u' + cp.toString(16).padStart(4, '0');
		else out += '\\U' + cp.toString(16).padStart(8, '0');
	}
	return out + '"';
}

const repo = process.argv[2];
if (!repo) {
	console.error('usage: node gen.js <repo-root>');
	process.exit(1);
}
const outDir = path.join(repo, 'internal/misc/csscolor');
const tc = require(path.join(process.cwd(), 'tinycolor.js'));

// 決定的な擬似乱数 (LCG)。
let seed = 20260826;
function rnd() {
	seed = (seed * 1103515245 + 12345) % 2147483648;
	return seed / 2147483648;
}
function pick(a) {
	return a[Math.floor(rnd() * a.length)];
}

// --- names.go -------------------------------------------------------------

const names = tc.names;
const nameKeys = Object.keys(names).sort();
let namesGo = '';
namesGo += '// Code generated from tinycolor2 (packages/backend/node_modules/tinycolor2).\n';
namesGo += '// DO NOT EDIT by hand: 再生成の手順は csscolor.go の package コメント。\n\n';
namesGo += 'package csscolor\n\n';
namesGo += '// names は tinycolor2 の色名テーブル (' + nameKeys.length + ' 件)。CSS の標準色名に\n';
namesGo += '// tinycolor 独自の `burntsienna` を足したもの。値は `#` 無しの hex で、\n';
namesGo += '// そのまま hex matcher へ渡す (tinycolor の stringInputToObject と同じ)。\n';
namesGo += 'var names = map[string]string{\n';
for (const k of nameKeys) namesGo += '\t"' + k + '": "' + names[k] + '",\n';
namesGo += '}\n';
fs.writeFileSync(path.join(outDir, 'names.go'), namesGo);

// --- vectors_test.go ------------------------------------------------------

const inputs = [
	// hex
	'#fff', '#FFF', 'fff', '#ffffff', 'ffffff', '#FFFFFF', '#abc', '#abcd', '#aabbcc', '#aabbccdd',
	'#000', '#123456', '#12345', '#1234567', '#12345678', '#1', '#12', '#', '', '#ff0000  ', '  #00ff00',
	'#f0f8ff', '0f0', '#0f0f', '#00ff00ff', '#GGG', '#gg0000',
	// names
	'red', 'RED', '  Red  ', 'rebeccapurple', 'burntsienna', 'transparent', 'TRANSPARENT',
	'aliceblue', 'white', 'black', 'blue', 'cyan', 'magenta', 'grey', 'gray', 'notacolor', '',
	// rgb
	'rgb(255,0,0)', 'rgb(0,255,0)', 'rgb(0, 0, 255)', 'rgb 255 0 0', 'rgb(255 0 0)', 'RGB(1,2,3)',
	'rgb(300,0,0)', 'rgb(-5,0,0)', 'rgb(255.5,0,0)', 'rgb(100%,0%,0%)', 'rgb(50%,50%,50%)',
	'rgb(1,2)', 'rgb(1,2,3,4)', 'rgba(255,0,0,0.5)', 'rgba(0,0,0,0)', 'rgba(1,2,3)', 'rgb(1.0,1.0,1.0)',
	'rgb(0.5,0.5,0.5)', 'rgb(+10,+20,+30)', 'rgb(-0,-0,-0)', 'rgb(1,2,3)garbage', 'garbage rgb(1,2,3)',
	// hsl
	'hsl(0,100%,50%)', 'hsl(120,100%,50%)', 'hsl(240,100%,50%)', 'hsl(0,100,50)', 'hsl(360,100%,50%)',
	'hsla(0,100%,50%,0.5)', 'hsl(0,0%,0%)', 'hsl(0,0%,100%)', 'hsl(180,50%,50%)', 'hsl(-60,100%,50%)',
	'hsl(0.5,0.5,0.5)', 'hsl(720,100%,50%)', 'hsl 200 50% 50%',
	// hsv
	'hsv(0,100%,100%)', 'hsv(120,100%,100%)', 'hsva(0,100%,100%,0.5)', 'hsv(0,0%,50%)', 'hsv(300,50%,50%)',
	'hsv(0.5,0.5,0.5)',
	// odd
	'  ', 'null', 'undefined', '#rgb', 'rgb()', 'rgb(,,)', 'hsl(,,)',
	'hsl(1.0,1.0,1.0)', 'hsv(1.0,1.0,1.0)', 'rgb(1.5,2.5,3.5)', 'rgb(0.4,0.6,0.5)',
	'hsl(359.9,99.9%,49.9%)', 'hsl(30,33.3%,66.6%)', 'hsva(10,20,30,40)', 'rgba(10%,20%,30%,40%)',
];

// **1e-6 より小さい値を必ず入れる。** JS の `String(number)` は 1e-6 まで固定
// 小数のままで、そこから指数表記へ切り替わる。`convertToPercentage` の戻り値が
// この境界をまたぐと `bound01` の読み方が変わるので、ここに小さい値が無いと
// 生成した vector は境界の実装差を素通しする (#2726 のレビュー指摘)。
const nums = () => pick([
	'0', '1', '2', '7', '15', '64', '127', '128', '180', '255', '256', '300', '359', '360', '720',
	'-1', '-60', '0.5', '1.0', '1.5', '33.3', '99.9', '0%', '1%', '50%', '99%', '100%', '150%', '-10%', '.5', '+7',
	'0.001', '0.0001', '0.00001', '0.000001', '0.0000009', '0.0000001', '0.000000001',
	'.0000001', '.000000001', '-0.0000001', '0.0000001%', '0.000000001%', '0.000001%',
	'0.00000000000001', '0.5000001', '0.9999999', '1.0000001', '255.0000001', '99.9999999',
	'100.0000001', '1.0000000000000002', '0.30000000000000004',
]);
const fns = ['rgb', 'rgba', 'hsl', 'hsla', 'hsv', 'hsva'];
// **区切り位置の空白も corpus に入れる。** JS の `\s` と Go の regexp の `\s` は
// 別物 (Go は `\v` も Unicode 空白も含まない) なので、区切りに Unicode 空白を
// 置く入力が無いと matcher 側の実装差を素通しする (#2726 のレビュー 2 周目)。
const seps = [
	',', ', ', ' ', '  ,',
	',\u00A0', '\u3000', ',\u3000', '\u000B', ',\u000B', '\u2028', ',\uFEFF',
	'\u2009', ',\u202F', '\u1680', ',\u2029', '\u205F',
];
for (let i = 0; i < 400; i++) {
	const f = pick(fns);
	const n = f.endsWith('a') ? 4 : 3;
	const parts = [];
	for (let j = 0; j < n; j++) parts.push(nums());
	const open = pick(['(', ' ']);
	inputs.push(f + open + parts.join(pick(seps)) + (open === '(' ? ')' : ''));
}
const hexd = '0123456789abcdefABCDEF';
for (let i = 0; i < 120; i++) {
	const len = pick([3, 4, 6, 8, 5, 7, 2]);
	let s = rnd() < 0.5 ? '#' : '';
	for (let j = 0; j < len; j++) s += hexd[Math.floor(rnd() * hexd.length)];
	inputs.push(s);
}
for (let i = 0; i < 40; i++) inputs.push(pick(nameKeys));

// `toLowerCase()` の差。Go の simple case mapping で ASCII に落ちるのは
// U+0130 (İ) だけ (全 Unicode を突き合わせた実測)。
for (const base of ['\u0130vory', '\u0130NDIANRED', 'RED\u0130', '\u0130']) inputs.push(base);

// 空白の trim。**JS の `\s` と Go の `unicode.IsSpace` は 2 つずれる**
// (U+FEFF は JS だけ、U+0085 は Go だけ) ので、両方を必ず corpus に入れる。
const ws = [
	'\uFEFF', '\u0085', '\u180E', '\u200B', '\u00A0', '\u1680', '\u2000', '\u2009',
	'\u200A', '\u2028', '\u2029', '\u202F', '\u205F', '\u3000', '\t', '\n', '\v', '\f', '\r', ' ',
];
for (const w of ws) {
	for (const base of ['red', '#f00', 'rgb(1,2,3)']) {
		inputs.push(w + base);
		inputs.push(base + w);
		inputs.push(w + base + w);
	}
}

const seen = new Set();
const out = [];
for (const s of inputs) {
	if (seen.has(s)) continue;
	seen.add(s);
	const c = new tc(s);
	out.push({ in: s, ok: c.isValid(), hex: c.isValid() ? c.toHexString() : '' });
}

let go = '';
go += '// Code generated from tinycolor2 (packages/backend/node_modules/tinycolor2).\n';
go += '// DO NOT EDIT by hand: 再生成の手順は csscolor.go の package コメント。\n\n';
go += 'package csscolor_test\n\n';
go += '// tinycolorVectors は**実物の tinycolor2 が返した値** (' + out.length + ' 件)。\n';
go += '// 手で書いた期待値ではないので、「upstream に揃えた」を推論で書かずに済む (#2726)。\n';
go += '// 入力は代表例に加えて、決定的な擬似乱数で組んだ関数形式 / hex / 色名を含む。\n';
go += '//\n';
go += '// **桁溢れ (400 桁の数値) はここに入れない** — 1 行が読めなくなるので\n';
go += '// csscolor_test.go の TestNormalize_HugeNumbers に手で置いてある。\n';
go += 'var tinycolorVectors = []struct {\n\tin  string\n\tok  bool\n\thex string\n}{\n';
for (const v of out) go += '\t{' + goString(v.in) + ', ' + v.ok + ', ' + goString(v.hex) + '},\n';
go += '}\n';
fs.writeFileSync(path.join(outDir, 'vectors_test.go'), go);

console.log('names:', nameKeys.length, 'vectors:', out.length);
