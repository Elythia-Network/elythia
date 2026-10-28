// Build the ajv conformance corpus for internal/api/paramschema.
//
// For every endpoint in the paramDef golden (extract.mjs), generate a valid
// input and inputs that break one thing at a time (a wrong type, a value just
// past a limit, a missing required property, two violations at once, ...),
// validate each with the same ajv version and options as upstream, and record
// the verdict, the first error (schemaPath and message) and, for accepted
// inputs, the value after ajv inserted the defaults.
//
// Usage (see `make paramdef-gen`):
//
//	node tools/paramdefspec/corpus.mjs --paramdefs <file> --synthetic <file> --node-modules <dir> --out <file>
//	node tools/paramdefspec/corpus.mjs --paramdefs <file> --elythia <file> --node-modules <dir> --out <file>
//
// With --elythia, the corpus is built for the endpoints of that file (Elythia's
// own paramDefs, elythia_paramdefs.json, #3869) instead, with the ajv version,
// options and formats of the upstream golden, and without synthetic schemas.
//
// synthetic.json adds hand-written schemas and inputs that probe keyword order
// and edge cases the upstream paramDefs do not reach; ajv judges them too, and
// a schema marked compileError must be one ajv refuses to compile.
//
// The output is JSON Lines: a header line, then for each endpoint a line
// naming it (with the schema, for a synthetic one) and one line per case. The input
// is embedded as raw JSON text (not re-serialized) so that number spellings
// such as 10.0 and 1e400 reach the Go side exactly as ajv parsed them. The
// generator is deterministic; regenerating from the same golden and ajv gives
// the same bytes.
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { parseArgs } from 'node:util';

const { values: args } = parseArgs({
	options: {
		paramdefs: { type: 'string' },
		synthetic: { type: 'string' },
		elythia: { type: 'string' },
		'node-modules': { type: 'string' },
		out: { type: 'string' },
	},
});
for (const k of ['paramdefs', 'node-modules', 'out']) {
	if (!args[k]) fail(`--${k} is required`);
}
if (!args.synthetic === !args.elythia) fail('give either --synthetic or --elythia');

function fail(msg) {
	console.error(`paramdefspec/corpus: ${msg}`);
	process.exit(1);
}

const goldenBytes = fs.readFileSync(args.paramdefs);
const golden = JSON.parse(goldenBytes.toString('utf8'));

// 本家と同じ版のajvを使う。frontend/の依存に推移的に入っているものを、
// pnpmの仮想ストアから版を名指しして読む(frontend/はajvに直接依存しない)。
const ajvDir = path.resolve(args['node-modules'], '.pnpm', `ajv@${golden.ajv.version}`, 'node_modules', 'ajv');
if (!fs.existsSync(path.join(ajvDir, 'package.json'))) {
	fail(`ajv ${golden.ajv.version} (the version upstream pins) is not installed at ${ajvDir}; it reaches frontend/node_modules only as a transitive dependency`);
}
const ajvPkg = JSON.parse(fs.readFileSync(path.join(ajvDir, 'package.json'), 'utf8'));
if (ajvPkg.version !== golden.ajv.version) fail(`${ajvDir} is ajv ${ajvPkg.version}, want ${golden.ajv.version}`);
const require = createRequire(import.meta.url);
const Ajv = require(ajvDir).default;
const ajv = new Ajv(golden.ajv.options);
for (const [name, { source, flags }] of Object.entries(golden.ajv.formats)) ajv.addFormat(name, new RegExp(source, flags));

// ---入力の組み立て---

// JSON.stringifyでは書けない数の綴り(10.0、1e400など)をそのまま出すための印。
class RawNum {
	constructor(text) {
		this.text = text;
	}
}

function serialize(v) {
	if (v instanceof RawNum) return v.text;
	if (Array.isArray(v)) return `[${v.map(serialize).join(',')}]`;
	if (v !== null && typeof v === 'object') return `{${Object.entries(v).map(([k, x]) => `${JSON.stringify(k)}:${serialize(x)}`).join(',')}}`;
	return JSON.stringify(v);
}

const isObj = (v) => v !== null && typeof v === 'object' && !Array.isArray(v) && !(v instanceof RawNum);

// JavaScriptの\sに入り、Goの\sに入らない文字と、その逆(U+0085はGoの
// unicode.IsSpaceだけが空白とみなす)、どちらでもない文字。
const SPACE_PROBES = [' ', 'a b', '\t', '\n', '\v', '\u00a0', '\u1680', '\u2000', '\u200a', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff', '\u0085', '\u200b', '\u180e', 'a\u3000', '\u3000a'];
const PATTERN_PROBES = ['abc', 'a', 'A_1', 'a-b', 'a.b', 'a/b', '*', 'a*', '#ffffff', '#FFF', '#fffffff', '#gggggg', '2000-01-01', '2000-1-1', '20000-01-01', 'image/png', 'image/*', 'x:y', 'a%b', 'a@b', 'é', '日本語', '😀', 'a\u0000', 'a'.repeat(20), 'a'.repeat(21), 'a'.repeat(256), 'a'.repeat(257), ...SPACE_PROBES];
const ID_PROBES = ['abc123', 'ABC', '0', 'a_b', 'a-b', 'a b', ' abc', 'abc\n', '日本', '\uff41\uff42\uff43', 'abc\u0000'];
const EMOJI = '\u{1F600}';

function sampleString(s) {
	let v = s.format === 'misskey:id' ? 'abc123' : 'hello';
	if (s.pattern) {
		const re = new RegExp(s.pattern, 'u');
		v = [v, ...PATTERN_PROBES].find((c) => re.test(c)) ?? v;
	}
	if (s.maxLength !== undefined && [...v].length > s.maxLength) v = [...v].slice(0, s.maxLength).join('');
	if (s.minLength !== undefined && [...v].length < s.minLength) v = v.padEnd(s.minLength, 'a');
	return v;
}

function sampleNumber(s) {
	if (s.minimum !== undefined) return s.minimum;
	if (s.maximum !== undefined) return Math.min(1, s.maximum);
	return 1;
}

function requiredOf(s) {
	const req = new Set(s.required ?? []);
	// notes/createはif/thenでtextを要求する。thenのrequiredも満たす。
	for (const k of s.then?.required ?? []) req.add(k);
	return req;
}

// sample returns a value the schema is expected to accept (best effort; the
// verdict always comes from ajv).
function sample(s, depth = 0) {
	if (!isObj(s) || depth > 8) return 'x';
	if (s.enum) return s.enum.find((v) => v !== null) ?? s.enum[0];
	// objectのnodeが持つanyOfは、分岐ごとに必須を分けるだけのことがある
	// (chat/messages/create、#3872)。そのときは、最初の分岐の必須を足したobjectにする。
	if (s.anyOf && s.type !== 'object' && !s.properties) return sample(s.anyOf[0], depth + 1);
	if (s.oneOf) return sample(s.oneOf[0], depth + 1);
	if (s.allOf) {
		const o = {};
		for (const b of s.allOf) {
			const v = sample(b, depth + 1);
			if (isObj(v)) Object.assign(o, v);
		}
		return o;
	}
	switch (s.type) {
		case 'string': return sampleString(s);
		case 'integer':
		case 'number': return sampleNumber(s);
		case 'boolean': return false;
		case 'null': return null;
		case 'array': return sampleArray(s, s.minItems ?? 0, depth);
		case 'object':
		case undefined: {
			if (s.type === undefined && !s.properties) return 'x';
			return sampleObject(s, s.anyOf?.[0], depth);
		}
	}
	return 'x';
}

// sampleObject returns an object with the required properties of s and, when
// given, of the anyOf branch b.
function sampleObject(s, b, depth) {
	const o = {};
	for (const k of requiredOf(s)) o[k] = sample(s.properties?.[k] ?? s.then?.properties?.[k], depth + 1);
	for (const k of isObj(b) ? requiredOf(b) : []) {
		if (!Object.hasOwn(o, k)) o[k] = sample(b.properties?.[k] ?? s.properties?.[k], depth + 1);
	}
	return o;
}

// sampleArray returns n distinct items for the array schema s.
function sampleArray(s, n, depth = 0) {
	const out = [];
	const seen = new Set();
	const item = s.items;
	for (let i = 0; out.length < n && i < n * 4 + 8; i++) {
		let v = sample(item, depth + 1);
		if (typeof v === 'string') v = i === 0 ? v : v + i;
		else if (typeof v === 'number') v = v + i;
		const key = serialize(v);
		if (s.uniqueItems && seen.has(key)) continue;
		seen.add(key);
		out.push(v);
	}
	return out;
}

// 型の取り違え。どの型でもnullは必ず試す(nullableの有無で結果が分かれる)。
// 残りは、その型の検査で紛れやすい値(整数に10.0や1e400、objectに配列)を
// 選ぶ。型の無いnode (anyOf / oneOfだけのもの)には全部を当てる。
// 型の無いnodeは1e400 (Infinity)も受理し、受理した値をJSONに書けないので外す。
const WRONG_TYPE = {
	string: [123, true, []],
	integer: [1.5, new RawNum('10.0'), new RawNum('1e1'), new RawNum('1e400'), new RawNum('-0'), '1', true],
	number: [1.5, new RawNum('1e400'), '1', true],
	boolean: [0, 'true'],
	object: [[], 'x'],
	array: [{}, 'x'],
	null: ['', 0],
	untyped: [true, 123, 1.5, new RawNum('10.0'), '', 'x-y', [], {}],
};

function pick(list) {
	return list.length <= 6 ? list : [...list.slice(0, 3), ...list.slice(-3)];
}

// mutate returns replacement values for a node with schema s whose current
// value is v (undefined when the property is absent). It recurses into
// properties, items and the branches of anyOf / oneOf / allOf / then.
function mutate(s, v, depth) {
	const out = [null, ...(WRONG_TYPE[isObj(s) && s.type ? s.type : 'untyped'] ?? WRONG_TYPE.untyped)];
	if (!isObj(s) || depth > 6) return out;
	if (s.enum) out.push(...pick(s.enum));
	if (s.type === 'string') {
		const L = s.maxLength;
		if (L !== undefined) {
			out.push('a'.repeat(L), 'a'.repeat(L + 1));
			if (L >= 1) out.push('a'.repeat(L - 1) + EMOJI, 'a'.repeat(L) + EMOJI);
		}
		const M = s.minLength;
		if (M !== undefined && M > 0) out.push('a'.repeat(M - 1), 'a'.repeat(M), EMOJI.repeat(M));
		out.push('', 'x-y');
		if (s.pattern) out.push(...PATTERN_PROBES);
		if (s.format === 'misskey:id') out.push(...ID_PROBES);
	}
	if (s.type === 'integer' || s.type === 'number') {
		if (s.minimum !== undefined) out.push(s.minimum, s.minimum - 1, s.minimum - 0.5, ...(Number.isInteger(s.minimum) ? [new RawNum(`${s.minimum}.0`)] : []));
		if (s.maximum !== undefined) out.push(s.maximum, s.maximum + 1, s.maximum + 0.5, ...(/e/.test(String(s.maximum)) ? [] : [new RawNum(`${s.maximum}e0`)]));
		out.push(-1);
	}
	if (s.type === 'boolean') out.push(false);
	if (s.type === 'array') {
		const k = s.minItems;
		if (k !== undefined && k > 0) out.push(sampleArray(s, k - 1), sampleArray(s, k));
		const K = s.maxItems;
		if (K !== undefined) out.push(sampleArray(s, K), sampleArray(s, K + 1));
		const two = sampleArray(s, Math.max(2, k ?? 0));
		if (two.length >= 2) {
			out.push([two[0], ...two]);
			out.push([two[0], two[1], two[0], ...two.slice(2)]);
		}
		if (s.items?.nullable) out.push([null, null], ['null', null]);
		out.push(['x-y'], [{ bad: 1 }], [null], ['__proto__', '__proto__']);
		const cur = Array.isArray(v) && v.length > 0 ? v : two.length ? two : [sample(s.items)];
		for (const r of mutate(s.items, cur[0], depth + 1)) out.push([r, ...cur.slice(1)]);
	}
	if (s.properties || s.type === 'object') {
		const cur = isObj(v) ? v : sample(s);
		if (isObj(cur)) out.push(...mutateObject(s, cur, depth));
	}
	for (const kw of ['anyOf', 'oneOf']) {
		for (const b of s[kw] ?? []) {
			// 必須を分けるだけの分岐 (chat/messages/create、#3872) は、その分岐の必須を
			// 満たすobjectを見本にし、親の項目を1つずつ壊す。分岐だけを見ると'x'になり、
			// 2つ目以降の分岐を通る入力が作られない。
			if (kw === 'anyOf' && (s.type === 'object' || s.properties) && isObj(b) && b.type === undefined && !b.properties) {
				const sb = sampleObject(s, b, depth);
				out.push(sb, ...mutateObject(s, sb, depth));
				continue;
			}
			const sb = sample(b);
			out.push(sb, ...mutate(b, sb, depth + 1));
		}
	}
	// oneOfの2つの枝を同時に満たす値(objectの枝なら、2つの見本を合わせたもの)。
	const [b0, b1] = s.oneOf ?? [];
	if (b0 && b1) {
		const s0 = sample(b0);
		const s1 = sample(b1);
		if (isObj(s0) && isObj(s1)) out.push({ ...s0, ...s1 });
	}
	for (const b of s.allOf ?? []) out.push(...mutate(b, isObj(v) ? v : sample(s), depth + 1));
	if (s.then) {
		const cur = isObj(v) ? v : sample(s);
		out.push(...mutate(s.then, cur, depth + 1));
		// ifが項目を要求するとき (emoji-application/createのkind: remote、#3875) は、
		// ifとthenの必須を満たす見本からもthenの項目を壊す。見本がifを満たさないと、
		// thenの検査に届く入力が1つも作られない。
		if (isObj(s.if) && requiredOf(s.if).size > 0) {
			const on = { ...cur };
			for (const k of [...requiredOf(s.if), ...requiredOf(s.then)]) {
				if (!Object.hasOwn(on, k)) on[k] = sample({ ...s.properties?.[k], ...s.if.properties?.[k], ...s.then.properties?.[k] }, depth + 1);
			}
			out.push(on, ...mutate(s.then, on, depth + 1));
		}
		// thenの項目は、ifが外れる入力でも試す。そうしないとthenの検査が先に報告され、
		// 外側のpropertiesの同じ項目の制限(notes/createのtextのmaxLengthなど)に届かない。
		const ifProp = Object.keys(s.if?.properties ?? {}).find((k) => s.properties?.[k]);
		if (ifProp !== undefined) {
			const off = { ...cur, [ifProp]: sample({ ...s.properties[ifProp], nullable: false, enum: undefined }) };
			for (const p of Object.keys(s.then.properties ?? {})) {
				if (s.properties?.[p]) for (const r of mutate(s.properties[p], off[p], depth + 1)) out.push({ ...off, [p]: r });
			}
		}
	}
	return out;
}

function mutateObject(s, cur, depth) {
	const out = [{ ...cur, unknownProperty: 1 }];
	const props = Object.keys(s.properties ?? {});
	for (const p of props) {
		if (Object.hasOwn(cur, p)) {
			const { [p]: _, ...rest } = cur;
			out.push(rest);
		}
		for (const r of mutate(s.properties[p], cur[p], depth + 1)) out.push({ ...cur, [p]: r });
		if (!Object.hasOwn(cur, p)) out.push({ ...cur, [p]: sample(s.properties[p]) });
	}
	// 2つの違反を同時に入れて、どちらが先に報告されるか(schemaの順か、入力の順か)を見る。
	const pairs = depth === 0 ? props.length - 1 : Math.min(props.length - 1, 3);
	for (let i = 0; i < pairs; i++) {
		out.push({ ...cur, [props[i]]: { bad: 1 }, [props[i + 1]]: { bad: 1 } });
		out.push({ ...cur, [props[i + 1]]: { bad: 1 }, [props[i]]: { bad: 1 } });
	}
	const req = [...requiredOf(s)];
	if (req.length > 0 && props.length > 0) {
		const rest = { ...cur };
		for (const r of req) delete rest[r];
		out.push(rest, { ...rest, [props[props.length - 1]]: { bad: 1 } });
	}
	return out;
}

// ---実行---

function hasNonFinite(v) {
	if (typeof v === 'number') return !Number.isFinite(v);
	if (Array.isArray(v)) return v.some(hasNonFinite);
	if (v !== null && typeof v === 'object') return Object.values(v).some(hasNonFinite);
	return false;
}

const lines = [];
let total = 0;
let accepted = 0;
let dropped = 0;

function runCases(validate, inputs) {
	const seen = new Set();
	for (const input of inputs) {
		const raw = input instanceof RawText ? input.text : serialize(input);
		if (seen.has(raw)) continue;
		seen.add(raw);
		const data = JSON.parse(raw);
		const ok = validate(data);
		// 受理した値にInfinity (1e400)が残ると、受理した後の値をJSONに書けない
		// (nullに化ける)ので、その入力は記録しない。拒否されるものは記録する。
		if (ok && hasNonFinite(data)) {
			dropped++;
			continue;
		}
		total++;
		if (ok) {
			accepted++;
			// defaultを入れても変わらなかったものはoutを省く(大きさを抑えるため)。
			const after = JSON.stringify(data);
			lines.push(after === JSON.stringify(JSON.parse(raw)) ? `{"in":${raw},"ok":true}` : `{"in":${raw},"ok":true,"out":${after}}`);
		} else {
			const e = validate.errors[0];
			lines.push(`{"in":${raw},"ok":false,"path":${JSON.stringify(e.schemaPath)},"msg":${JSON.stringify(e.message)}}`);
		}
	}
}

// 手で書いた入力。数の綴り(1.0)を保つため、JSONの文字列のまま使う。1行に収める
// ので、空白を含めない。
class RawText {
	constructor(text) {
		if (/\s/.test(text.replace(/"(?:[^"\\]|\\.)*"/g, '""'))) fail(`synthetic input must be compact JSON: ${text}`);
		JSON.parse(text);
		this.text = text;
	}
}

// --elythiaのときは、本家のgoldenからはajvの版・設定・形式だけを使う。
const elythiaBytes = args.elythia ? fs.readFileSync(args.elythia) : null;
const endpoints = elythiaBytes ? JSON.parse(elythiaBytes.toString('utf8')).endpoints : golden.endpoints;
for (const ep of Object.keys(endpoints).sort()) {
	const pd = endpoints[ep].paramDef;
	const base = sample(pd);
	lines.push(`{"ep":${JSON.stringify(ep)}}`);
	runCases(ajv.compile(pd), [base, {}, null, [], 'x', 1, true, new RawNum('1.0'), ...mutate(pd, base, 0)]);
}

// 本家のparamDefには無い形で、keywordの順や組み合わせの境界をajvに判定させる
// (synthetic.json)。ajvの判定を正解にするので、Go側の思い込みをテストに書かずに済む。
// 本家と同じ設定にlogger: falseだけを足す(strictTypesの警告を出さないため。判定は変わらない)。
const quietAjv = new Ajv({ ...golden.ajv.options, logger: false });
for (const [name, { source, flags }] of Object.entries(golden.ajv.formats)) quietAjv.addFormat(name, new RegExp(source, flags));
const synthetic = args.synthetic ? JSON.parse(fs.readFileSync(args.synthetic, 'utf8')) : [];
for (const { name, paramDef, inputs, compileError } of synthetic) {
	const ep = `synthetic:${name}`;
	let validate;
	try {
		validate = quietAjv.compile(paramDef);
	} catch (e) {
		if (!compileError) fail(`${ep}: ajv rejects the schema: ${e.message}`);
		lines.push(`{"ep":${JSON.stringify(ep)},"paramDef":${JSON.stringify(paramDef)},"compileError":${JSON.stringify(e.message)}}`);
		continue;
	}
	if (compileError) fail(`${ep}: ajv compiles the schema`);
	lines.push(`{"ep":${JSON.stringify(ep)},"paramDef":${JSON.stringify(paramDef)}}`);
	runCases(validate, [...inputs.map((t) => new RawText(t)), ...mutate(paramDef, sample(paramDef), 0)]);
}

const header = {
	misskeyVersion: golden.misskeyVersion,
	ajv: golden.ajv.version,
	// どのparamDefのgoldenから作ったか。Goのテストがこれを見て、goldenだけを
	// 作り直してcorpusを作り直し忘れた状態を落とす。
	paramdefsSha256: crypto.createHash('sha256').update(goldenBytes).digest('hex'),
	// Elythiaの宣言から作ったときは、その宣言のhashも書く。
	...(elythiaBytes ? { elythiaParamdefsSha256: crypto.createHash('sha256').update(elythiaBytes).digest('hex') } : {}),
	cases: total,
};
fs.writeFileSync(args.out, `${JSON.stringify(header)}\n${lines.join('\n')}\n`);
console.log(`paramdefspec/corpus: ${total} cases (${accepted} accepted; ${dropped} accepted with Infinity not recorded) -> ${args.out}`);
