// Extract the paramDef (and the meta keys the API layer reads) of every
// upstream Misskey endpoint into one JSON golden, without executing upstream
// code: each endpoint's object literal is read from its TypeScript AST and
// evaluated, following imports to resolve the constants it references.
//
// The golden also records how upstream configures ajv (endpoint-base.ts):
// the ajv version from packages/backend/package.json, the constructor options
// and the formats added with addFormat. corpus.mjs builds its ajv from these,
// and internal/api/paramschema checks that it implements exactly them.
//
// Usage (see `make paramdef-gen`):
//
//	node tools/paramdefspec/extract.mjs --typescript <dir> --upstream <dir> --out <file>
//
// Any expression the evaluator cannot resolve fails the run: the golden must
// describe what ajv receives, not an approximation of it. Only top-level const
// declarations are resolved; a let / var, or a name the file writes after its
// declaration (assignment, ++, delete, Object.assign, push, ...), fails too.
// The write detection is by name and has limits; see writtenNames.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { parseArgs } from 'node:util';

const { values: args } = parseArgs({
	options: {
		typescript: { type: 'string' },
		upstream: { type: 'string' },
		out: { type: 'string' },
	},
});
for (const k of ['typescript', 'upstream', 'out']) {
	if (!args[k]) fail(`--${k} is required`);
}

const require = createRequire(import.meta.url);
const ts = require(path.resolve(args.typescript));
const ROOT = path.resolve(args.upstream);
const BACKEND = path.join(ROOT, 'packages/backend');
const SRC = path.join(BACKEND, 'src');
const API = path.join(SRC, 'server/api');

// metaのうちAPIの層が読むものだけを残す。rate limit (`ms('1hour')`)と
// res (`getJsonSchema`)は引数の検証に関係せず、解決もしないので落とす。
const META_KEYS = ['requireCredential', 'requireAdmin', 'requireModerator', 'requireFile', 'allowGet', 'cacheSec', 'secure', 'kind', 'prohibitMoved', 'requireRolePolicy'];

function fail(msg) {
	console.error(`paramdefspec/extract: ${msg}`);
	process.exit(1);
}

const fileCache = new Map();
function load(file) {
	if (fileCache.has(file)) return fileCache.get(file);
	let text;
	try {
		text = fs.readFileSync(file, 'utf8');
	} catch {
		fileCache.set(file, null);
		return null;
	}
	const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
	const consts = new Map();
	// const でない宣言 (let / var) は、初期値が実行時の値とは限らないので解決しない。
	const mutable = new Set();
	const imports = new Map();
	for (const st of sf.statements) {
		if (ts.isVariableStatement(st)) {
			const isConst = (st.declarationList.flags & ts.NodeFlags.Const) !== 0;
			for (const d of st.declarationList.declarations) {
				if (!ts.isIdentifier(d.name)) continue;
				if (!isConst) mutable.add(d.name.text);
				else if (d.initializer) consts.set(d.name.text, d.initializer);
			}
		} else if (ts.isImportDeclaration(st) && st.importClause?.namedBindings && ts.isNamedImports(st.importClause.namedBindings)) {
			const spec = st.moduleSpecifier.text;
			for (const el of st.importClause.namedBindings.elements) {
				imports.set(el.name.text, { spec, name: (el.propertyName ?? el.name).text });
			}
		} else if (ts.isExportDeclaration(st) && st.exportClause && ts.isNamedExports(st.exportClause) && st.moduleSpecifier) {
			for (const el of st.exportClause.elements) {
				imports.set(el.name.text, { spec: st.moduleSpecifier.text, name: (el.propertyName ?? el.name).text });
			}
		}
	}
	const mod = { file, sf, consts, mutable, imports, written: writtenNames(sf) };
	fileCache.set(file, mod);
	return mod;
}

// 配列やMapを書き換えるメソッド。`X.push(...)`のXを書き換えられたものとみなす。
const MUTATING_METHODS = new Set(['push', 'pop', 'shift', 'unshift', 'splice', 'sort', 'reverse', 'fill', 'copyWithin', 'set', 'add', 'delete', 'clear']);
const MUTATING_CALLS = new Set(['Object.assign', 'Object.defineProperty', 'Object.defineProperties', 'Object.setPrototypeOf', 'Reflect.set', 'Reflect.defineProperty', 'Reflect.deleteProperty']);

// writtenNames collects the names whose value (or something reachable from
// it) the file writes after the declaration: assignments, ++ / --, delete,
// mutating calls. The names are not scope-resolved, so a local variable that
// shadows a top-level one also counts, and so does a read that looks like a
// write (`[...X].sort()`, a key or default inside a destructuring); that only
// makes the run fail.
//
// **宣言の後の書き換えを見落とすと、goldenが実行時の値と黙って違う(#3673)。**
// 見つけた名前を参照する値は解決せず、全体を落とす。
//
// **名前で見るので、見つけられない書き換えがある。** 次のものは落ちずに、
// goldenが実行時の値と違ったままになる。2026.10.0では、本家の.tsを実行して得た
// paramDefとgoldenが一致した(#3673のレビューで確認)が、増えても気付けないので、
// 本家の版を上げたときは差分の中にparamDefへの書き換えが無いかを読む
// (docs/upstream-catch-up.md)。
//   - 別名を通した書き換え(`const p = paramDef.properties; p.limit.maximum = 7`、
//     分割代入で受けた名前への書き換え)
//   - 関数やcallbackの引数として渡した先での書き換え
//     (`function w(o) { o.maximum = 7 } w(paramDef.properties.limit)`、
//     `Object.values(paramDef.properties).forEach((p) => { ... })`)
//   - 名前で書かない呼び出し(`Object['assign'](...)`、`(0, Object.assign)(...)`、
//     `Array.prototype.push.call(X, ...)`)
//   - 定義のファイルとendpointのファイル以外(第3のファイル)からの書き換え
function writtenNames(sf) {
	const names = new Set();
	const addTarget = (e) => {
		while (e && (ts.isParenthesizedExpression(e) || ts.isAsExpression(e) || ts.isNonNullExpression(e) || ts.isSatisfiesExpression(e) || ts.isPropertyAccessExpression(e) || ts.isElementAccessExpression(e))) e = e.expression;
		if (!e) return;
		if (ts.isIdentifier(e)) names.add(e.text);
		else if (ts.isArrayLiteralExpression(e) || ts.isObjectLiteralExpression(e)) {
			// 分割代入の左辺。
			const visit = (n) => {
				if (ts.isIdentifier(n)) names.add(n.text);
				ts.forEachChild(n, visit);
			};
			visit(e);
		}
	};
	const visit = (n) => {
		if (ts.isBinaryExpression(n) && n.operatorToken.kind >= ts.SyntaxKind.FirstAssignment && n.operatorToken.kind <= ts.SyntaxKind.LastAssignment) addTarget(n.left);
		else if ((ts.isPrefixUnaryExpression(n) || ts.isPostfixUnaryExpression(n)) && (n.operator === ts.SyntaxKind.PlusPlusToken || n.operator === ts.SyntaxKind.MinusMinusToken)) addTarget(n.operand);
		else if (ts.isDeleteExpression(n)) addTarget(n.expression);
		else if ((ts.isForInStatement(n) || ts.isForOfStatement(n)) && !ts.isVariableDeclarationList(n.initializer)) addTarget(n.initializer);
		else if (ts.isCallExpression(n)) {
			if (MUTATING_CALLS.has(n.expression.getText()) && n.arguments.length > 0) addTarget(n.arguments[0]);
			else if (ts.isPropertyAccessExpression(n.expression) && MUTATING_METHODS.has(n.expression.name.text)) addTarget(n.expression.expression);
		}
		ts.forEachChild(n, visit);
	};
	visit(sf);
	return names;
}

function resolveSpec(fromFile, spec) {
	let p;
	if (spec.startsWith('@/')) p = path.join(SRC, spec.slice(2));
	else if (spec.startsWith('.')) p = path.join(path.dirname(fromFile), spec);
	else if (spec === 'misskey-js') p = path.join(ROOT, 'packages/misskey-js/src/index.ts');
	else if (spec.startsWith('misskey-js/')) p = path.join(ROOT, 'packages/misskey-js/src', spec.slice('misskey-js/'.length));
	else return null;
	p = p.replace(/\.js$/, '.ts');
	if (!p.endsWith('.ts')) p = fs.existsSync(p + '.ts') ? p + '.ts' : path.join(p, 'index.ts');
	return p;
}

function lookup(mod, name, depth = 0) {
	if (!mod || depth > 8) return undefined;
	if (mod.written.has(name)) return { bad: `${name} is written after its declaration in ${path.relative(ROOT, mod.file)}` };
	if (mod.mutable.has(name)) return { bad: `${name} is not a const in ${path.relative(ROOT, mod.file)}` };
	if (mod.consts.has(name)) return { mod, node: mod.consts.get(name) };
	const im = mod.imports.get(name);
	if (im) {
		const f = resolveSpec(mod.file, im.spec);
		if (f) return lookup(load(f), im.name, depth + 1);
	}
	for (const st of mod.sf.statements) {
		if (ts.isExportDeclaration(st) && !st.exportClause && st.moduleSpecifier) {
			const f = resolveSpec(mod.file, st.moduleSpecifier.text);
			const r = f && lookup(load(f), name, depth + 1);
			if (r) return r;
		}
	}
	return undefined;
}

// 解決できなかった式は、この印を値に残す。出力の前に全体を走査して、1つでも
// 残っていたら落とす(印の付いた値をajvに渡すことはできないため)。
class Unresolved {
	constructor(text) {
		this.text = text;
	}
}
const UNDEFINED = Symbol('undefined');

function ev(mod, n) {
	while (ts.isAsExpression(n) || ts.isSatisfiesExpression(n) || ts.isParenthesizedExpression(n) || (ts.isTypeAssertionExpression && ts.isTypeAssertionExpression(n))) n = n.expression;
	if (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)) return n.text;
	if (ts.isNumericLiteral(n)) return Number(n.text);
	if (ts.isPrefixUnaryExpression(n) && n.operator === ts.SyntaxKind.MinusToken) {
		const v = ev(mod, n.operand);
		return typeof v === 'number' ? -v : new Unresolved(n.getText());
	}
	if (ts.isBinaryExpression(n)) {
		const l = ev(mod, n.left);
		const r = ev(mod, n.right);
		if (typeof l === 'number' && typeof r === 'number') {
			switch (n.operatorToken.kind) {
				case ts.SyntaxKind.AsteriskToken: return l * r;
				case ts.SyntaxKind.PlusToken: return l + r;
				case ts.SyntaxKind.MinusToken: return l - r;
				case ts.SyntaxKind.SlashToken: return l / r;
			}
		}
		return new Unresolved(n.getText());
	}
	if (n.kind === ts.SyntaxKind.TrueKeyword) return true;
	if (n.kind === ts.SyntaxKind.FalseKeyword) return false;
	if (n.kind === ts.SyntaxKind.NullKeyword) return null;
	if (ts.isRegularExpressionLiteral(n)) return new Unresolved(n.text);
	if (ts.isArrayLiteralExpression(n)) {
		const a = [];
		for (const e of n.elements) {
			if (ts.isSpreadElement(e)) {
				const v = ev(mod, e.expression);
				if (Array.isArray(v)) a.push(...v);
				else a.push(new Unresolved(e.getText()));
			} else {
				a.push(ev(mod, e));
			}
		}
		return a;
	}
	if (ts.isObjectLiteralExpression(n)) {
		const o = {};
		for (const p of n.properties) {
			if (ts.isPropertyAssignment(p)) {
				let k;
				if (ts.isIdentifier(p.name) || ts.isStringLiteral(p.name) || ts.isNumericLiteral(p.name)) k = p.name.text;
				else if (ts.isComputedPropertyName(p.name)) {
					const kv = ev(mod, p.name.expression);
					if (typeof kv !== 'string') return new Unresolved(n.getText());
					k = kv;
				} else return new Unresolved(n.getText());
				const v = ev(mod, p.initializer);
				// `key: undefined`は、ajvから見るとkeyが無いのと同じ。
				if (v === UNDEFINED) delete o[k];
				else o[k] = v;
			} else if (ts.isShorthandPropertyAssignment(p)) {
				o[p.name.text] = ev(mod, p.name);
			} else if (ts.isSpreadAssignment(p)) {
				const v = ev(mod, p.expression);
				if (v && typeof v === 'object' && !Array.isArray(v) && !(v instanceof Unresolved)) Object.assign(o, v);
				else return new Unresolved(p.getText());
			} else {
				return new Unresolved(p.getText());
			}
		}
		return o;
	}
	if (ts.isIdentifier(n)) {
		if (n.text === 'undefined') return UNDEFINED;
		const r = lookup(mod, n.text);
		if (r?.bad) return new Unresolved(r.bad);
		if (r) return ev(r.mod, r.node);
		return new Unresolved(n.text);
	}
	if (ts.isPropertyAccessExpression(n)) {
		// 正規表現の文字列は`/.../.source`で書かれていることがある。
		if (n.name.text === 'source' && ts.isRegularExpressionLiteral(n.expression)) return regexBody(n.expression);
		const base = ev(mod, n.expression);
		if (base && typeof base === 'object' && !(base instanceof Unresolved) && Object.hasOwn(base, n.name.text)) return base[n.name.text];
		return new Unresolved(n.getText());
	}
	if (ts.isCallExpression(n)) {
		const callee = n.expression.getText();
		// `/.../.toString().slice(1, -1)` (flagsの無い正規表現の本体)。
		const m = /^(.*)\.toString\(\)\.slice\(1, -1\)$/s.exec(n.getText());
		if (m) {
			const inner = n.expression.expression?.expression?.expression;
			if (inner && ts.isRegularExpressionLiteral(inner) && inner.text.endsWith('/')) return regexBody(inner);
		}
		if ((callee === 'Object.keys' || callee === 'Object.values') && n.arguments.length === 1) {
			const v = ev(mod, n.arguments[0]);
			if (v && typeof v === 'object' && !(v instanceof Unresolved)) return callee === 'Object.keys' ? Object.keys(v) : Object.values(v);
		}
	}
	return new Unresolved(n.getText().slice(0, 200));
}

function regexBody(lit) {
	const t = lit.text;
	return t.slice(1, t.lastIndexOf('/'));
}

function findUnresolved(v, where, out) {
	if (v instanceof Unresolved) out.push(`${where}: ${v.text}`);
	else if (v === UNDEFINED) out.push(`${where}: undefined`);
	else if (Array.isArray(v)) v.forEach((x, i) => findUnresolved(x, `${where}[${i}]`, out));
	else if (v && typeof v === 'object') for (const [k, x] of Object.entries(v)) findUnresolved(x, `${where}.${k}`, out);
	else if (typeof v === 'number' && !Number.isFinite(v)) out.push(`${where}: ${v}`);
}

// --- endpointの一覧(endpoint-list.tsの`export * as 'name' from '...'`) ---
function listEndpoints() {
	const file = path.join(API, 'endpoint-list.ts');
	const mod = load(file);
	if (!mod) fail(`cannot read ${file} (run make upstream-fetch)`);
	const list = [];
	for (const st of mod.sf.statements) {
		if (ts.isExportDeclaration(st) && st.exportClause && ts.isNamespaceExport(st.exportClause) && st.moduleSpecifier) {
			const name = st.exportClause.name.text;
			const f = resolveSpec(file, st.moduleSpecifier.text);
			list.push({ name, file: f });
		}
	}
	return list;
}

// --- ajvの設定(endpoint-base.tsの`new Ajv({...})`と`ajv.addFormat(...)`) ---
function readAjvConfig() {
	const file = path.join(API, 'endpoint-base.ts');
	const mod = load(file);
	if (!mod) fail(`cannot read ${file}`);
	let options;
	let constructions = 0;
	const formats = {};
	const visit = (node) => {
		if (ts.isNewExpression(node) && node.expression.getText() === 'Ajv') {
			constructions++;
			options = node.arguments?.length ? ev(mod, node.arguments[0]) : {};
		}
		if (ts.isCallExpression(node) && ts.isPropertyAccessExpression(node.expression) && node.expression.name.text === 'addFormat') {
			const [nameNode, reNode] = node.arguments;
			if (!nameNode || !ts.isStringLiteral(nameNode) || !reNode || !ts.isRegularExpressionLiteral(reNode)) fail(`unsupported addFormat call: ${node.getText()}`);
			const t = reNode.text;
			formats[nameNode.text] = { source: t.slice(1, t.lastIndexOf('/')), flags: t.slice(t.lastIndexOf('/') + 1) };
		}
		ts.forEachChild(node, visit);
	};
	visit(mod.sf);
	if (constructions !== 1) fail(`expected exactly one new Ajv(...) in ${file}, found ${constructions}`);
	const pkg = JSON.parse(fs.readFileSync(path.join(BACKEND, 'package.json'), 'utf8'));
	const version = pkg.dependencies?.ajv;
	if (!/^\d+\.\d+\.\d+$/.test(version ?? '')) fail(`packages/backend/package.json must pin ajv to an exact version, got ${version}`);
	return { version, options, formats };
}

const rootPkg = JSON.parse(fs.readFileSync(path.join(ROOT, 'package.json'), 'utf8'));
const ajv = readAjvConfig();
const endpoints = {};
const problems = [];
findUnresolved(ajv, 'ajv', problems);
for (const { name, file } of listEndpoints()) {
	const mod = load(file);
	if (!mod) {
		problems.push(`${name}: cannot read ${file}`);
		continue;
	}
	const pdRef = lookup(mod, 'paramDef');
	const metaRef = lookup(mod, 'meta');
	if (!pdRef || !metaRef) {
		problems.push(`${name}: paramDef or meta not found`);
		continue;
	}
	if (pdRef.bad || metaRef.bad) {
		problems.push(`${name}: ${pdRef.bad ?? metaRef.bad}`);
		continue;
	}
	const paramDef = ev(pdRef.mod, pdRef.node);
	const fullMeta = ev(metaRef.mod, metaRef.node);
	if (!fullMeta || typeof fullMeta !== 'object' || fullMeta instanceof Unresolved) {
		problems.push(`${name}: meta is not an object literal`);
		continue;
	}
	const meta = {};
	for (const k of META_KEYS) if (Object.hasOwn(fullMeta, k)) meta[k] = fullMeta[k];
	findUnresolved(paramDef, `${name}.paramDef`, problems);
	findUnresolved(meta, `${name}.meta`, problems);
	endpoints[name] = { paramDef, meta };
}
if (problems.length) fail(`unresolved values:\n  ${problems.join('\n  ')}`);
const names = Object.keys(endpoints).sort();
if (names.length === 0) fail('no endpoints found');

// 本家の版を上げたときの差分がendpoint単位で読めるように、endpointごとに1行で書く。
// 項目の順はJSのオブジェクトの順(= ajvがpropertiesを回す順)のまま残す。
let out = '{\n';
out += `"misskeyVersion": ${JSON.stringify(rootPkg.version)},\n`;
out += `"ajv": ${JSON.stringify(ajv)},\n`;
out += '"endpoints": {\n';
out += names.map((n) => `${JSON.stringify(n)}: ${JSON.stringify(endpoints[n])}`).join(',\n');
out += '\n}\n}\n';
fs.writeFileSync(args.out, out);
console.log(`paramdefspec/extract: ${names.length} endpoints (Misskey ${rootPkg.version}, ajv ${ajv.version}) -> ${args.out}`);
