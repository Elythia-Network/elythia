// Regenerates emoji_mfmjs.json, the expected trees of TestParse_UnicodeEmojiMatchesMfmJs,
// by running mfm-js on every entry of the emoji-data list that mfm-js depends on.
//
//   node internal/activitypub/mfm/testdata/emoji_mfmjs.mjs third_party/misskey > internal/activitypub/mfm/testdata/emoji_mfmjs.json
//
// The serialization matches serializeTree in mention_mfmjs_test.go.
import fs from 'node:fs';
import path from 'node:path';

const misskey = process.argv[2] ?? 'third_party/misskey';
const mfmjsDir = fs.realpathSync(path.join(misskey, 'packages/frontend/node_modules/mfm-js'));
const dataDir = path.join(path.dirname(mfmjsDir), '@misskey-dev/emoji-data');
const mfm = await import(path.join(mfmjsDir, 'built/index.mjs'));
const dataVersion = JSON.parse(fs.readFileSync(path.join(dataDir, 'package.json'), 'utf8')).version;
const mfmjsVersion = JSON.parse(fs.readFileSync(path.join(mfmjsDir, 'package.json'), 'utf8')).version;
const list = JSON.parse(fs.readFileSync(path.join(dataDir, 'built/emojilist.json'), 'utf8')).map(e => e[0]);

const ser = (nodes) => nodes.map(n => {
	const p = n.props || {};
	let head = n.type;
	if (n.type === 'text') head += ':' + p.text;
	else if (n.type === 'mention') head += ':' + p.acct;
	else if (n.type === 'emojiCode') head += ':' + p.name;
	else if (n.type === 'unicodeEmoji') head += ':' + p.emoji;
	else if (n.type === 'hashtag') head += ':' + p.hashtag;
	else if (n.type === 'url') head += ':' + p.url + (p.brackets ? '<>' : '');
	else if (n.type === 'link') head += ':' + p.url + (p.silent ? '!' : '');
	return n.children && n.children.length ? head + '[' + ser(n.children) + ']' : head;
}).join('|');

// 一覧の各絵文字を、単独・2 つ並べたもの・行頭の検索構文・U+FE0F を外したもの・
// 先頭の文字の後ろに U+FE0E を挟んだもの (否定の先読みの分岐) で読ませる。
const inputs = [];
for (const e of list) {
	const stripped = e.replaceAll('\ufe0f', '');
	const [first, ...rest] = [...e];
	inputs.push(e, e + e, e + ' 検索', stripped, first + '\ufe0e' + rest.join(''));
}
const cases = [...new Set(inputs)].map(s => [s, ser(mfm.parse(s))]);
// 差分を読めるように 1 行に 1 件ずつ書く
process.stdout.write(`{"mfmjs":${JSON.stringify(mfmjsVersion)},"emojiData":${JSON.stringify(dataVersion)},"cases":[\n`);
process.stdout.write(cases.map(c => JSON.stringify(c)).join(',\n'));
process.stdout.write('\n]}\n');
