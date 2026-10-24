// Write the /api.json that upstream Misskey serves, by calling upstream's own
// genOpenapiSpec in its official image (`make openapi-upstream-check`).
// internal/api/openapi compares its document with it.
//
// Usage (inside misskey/misskey:<version>, as the image's node):
//
//	node upstream-apijson.mjs <out>
//
// genOpenapiSpecが読むconfigはversionとapiUrlだけで、どちらも比べる対象に
// 入らない。/api.jsonと同じくincludeSelfRefはfalseで呼ぶ(OpenApiServerService.ts)。
import fs from 'node:fs';
import { createRequire } from 'node:module';

const out = process.argv[2];
if (!out) {
	console.error('upstream-apijson: output path is required');
	process.exit(1);
}
// 2026.10.1からは、gen-specのchunkがNestの部品を読み込み、その時点で
// reflect-metadataが要る(本家のbootは先に読み込んでいる)。backendの
// node_modulesから読む。
createRequire('/misskey/packages/backend/package.json')('reflect-metadata');
const { genOpenapiSpec } = await import('/misskey/packages/backend/built/gen-spec.js');
const spec = genOpenapiSpec({ version: '0.0.0', apiUrl: 'https://example.test/api' });
fs.writeFileSync(out, JSON.stringify(spec));
console.log(`upstream-apijson: ${Object.keys(spec.paths).length} paths, ${Object.keys(spec.components.schemas).length} schemas -> ${out}`);
