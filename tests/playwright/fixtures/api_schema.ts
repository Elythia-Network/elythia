/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia 独自の API の応答を、/api.json の宣言の型で照合する (#4093)。
//
// - 型は実行中のサーバーの /api.json から取る。宣言 (internal/api/paramschema/
//   elythia_apidoc.json) から組み立てたものなので、宣言と応答の食い違いがそのまま落ちる
// - date-time は本家の書式 (ミリ秒 3 桁と Z) で判定する。ajv-formats の date-time は
//   Go の既定の書式 (RFC3339Nano) も通し、#4091 のずれを見つけられないため。有り得ない
//   日時は、ajv-formats の判定でも落とす
// - Elythia 独自の型 (独自の operation の応答と、そこからしか引かれない components) だけ、
//   宣言に無いフィールドを落とす。本家の型は Elythia が欄を足しているので落とさない

import type { APIRequestContext, APIResponse } from '@playwright/test';
import { expect } from '@playwright/test';
import Ajv2020, { type ErrorObject, type ValidateFunction } from 'ajv/dist/2020';
import addFormats from 'ajv-formats';
import { baseURL } from './api';

/** The tag /api.json puts on the Elythia endpoints. */
export const ELYTHIA_TAG = 'Elythia';

/** The format of dates upstream Misskey sends (`Date.prototype.toISOString`). */
export const UPSTREAM_DATE_TIME = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/;

// 独自の型の component に付ける、閉じない版の名前の接尾辞。allOf の枝として引くときに使う
const OPEN_SUFFIX = '__open';
const COMPONENT_PREFIX = '#/components/schemas/';
const DEFS_ID = 'components';
// これだけを持つ object か配列の schema は、値の形を何も決めていない (`{}` や `{ type: 'object' }`)
const UNTYPED_KEYS = new Set(['type', 'description', 'example']);

type Schema = { [k: string]: unknown };

interface Operation {
  tags?: string[];
  responses?: Record<string, { description?: string; content?: Record<string, { schema?: Schema }> }>;
}

interface OpenAPIDoc {
  paths: Record<string, { post?: Operation }>;
  components: { schemas: Record<string, Schema> };
}

/** What a response of an endpoint is checked against. */
export type ExpectedResponse = { status: 204 } | { status: 200; strict: ValidateFunction; loose: ValidateFunction; schema: Schema };

/** The compiled /api.json: the Elythia endpoints and their response validators. */
export interface ResponseSchemas {
  /** The Elythia endpoints in /api.json, without the leading slash. */
  elythiaEndpoints: string[];
  /** The components only the Elythia endpoints refer to. */
  elythiaComponents: Set<string>;
  expected(endpoint: string): ExpectedResponse;
}

function isObject(v: unknown): v is Schema {
  return typeof v === 'object' && v != null && !Array.isArray(v);
}

function refName(ref: unknown): string | undefined {
  return typeof ref === 'string' && ref.startsWith(COMPONENT_PREFIX) ? ref.slice(COMPONENT_PREFIX.length) : undefined;
}

/** Collects the component names a schema refers to, following the refs. */
function collectRefs(node: unknown, components: Record<string, Schema>, into: Set<string>): void {
  if (Array.isArray(node)) {
    for (const v of node) collectRefs(v, components, into);
    return;
  }
  if (!isObject(node)) return;
  const name = refName(node.$ref);
  if (name != null && !into.has(name)) {
    into.add(name);
    collectRefs(components[name], components, into);
  }
  for (const v of Object.values(node)) collectRefs(v, components, into);
}

/**
 * Splits the components into the ones the Elythia endpoints alone refer to.
 * A component an upstream endpoint also refers to is an upstream type.
 */
export function elythiaOnlyComponents(doc: OpenAPIDoc): Set<string> {
  const upstream = new Set<string>();
  const elythia = new Set<string>();
  for (const item of Object.values(doc.paths)) {
    const op = item.post;
    if (op == null) continue;
    collectRefs(op, doc.components.schemas, op.tags?.includes(ELYTHIA_TAG) ? elythia : upstream);
  }
  for (const name of upstream) elythia.delete(name);
  return elythia;
}

/**
 * Rewrites a schema of an Elythia type so that a property it does not declare
 * fails. `open` leaves the top object open, for a branch of allOf: the
 * properties of the other branches are evaluated only at the parent.
 */
export function closeSchema(node: unknown, elythia: Set<string>, open = false): unknown {
  if (Array.isArray(node)) return node.map((v) => closeSchema(v, elythia));
  if (!isObject(node)) return node;
  const out: Schema = {};
  for (const [k, v] of Object.entries(node)) {
    if (k === 'allOf' && Array.isArray(v)) {
      // 枝は閉じない版で引き、閉じるのは親でまとめて行う。枝を閉じると、ほかの枝の欄を落とす
      out.allOf = v.map((b) => {
        if (isObject(b) && refName(b.$ref) != null && elythia.has(refName(b.$ref)!)) return { ...b, $ref: `${b.$ref}${OPEN_SUFFIX}` };
        return closeSchema(b, elythia, true);
      });
    } else if (k === 'properties' && isObject(v)) {
      out.properties = Object.fromEntries(Object.entries(v).map(([p, s]) => [p, closeSchema(s, elythia)]));
    } else if (k === 'example' || k === 'enum' || k === 'const' || k === 'required') {
      out[k] = v;
    } else {
      out[k] = closeSchema(v, elythia);
    }
  }
  if (open || 'additionalProperties' in node || 'unevaluatedProperties' in node) return out;
  if (Array.isArray(node.allOf)) {
    // 本家の型を枝に持つ allOf は閉じない。本家の型には Elythia が欄を足している
    const allElythia = node.allOf.every((b) => !isObject(b) || refName(b.$ref) == null || elythia.has(refName(b.$ref)!));
    if (allElythia) out.unevaluatedProperties = false;
  } else if (isObject(node.properties)) {
    out.unevaluatedProperties = false;
  }
  return out;
}

/** Whether a schema leaves the shape of an object or array open: `{}`, `{ type: 'object' }`. */
function isUntyped(schema: Schema): boolean {
  return [undefined, 'object', 'array'].includes(schema.type as string | undefined) && Object.keys(schema).every((k) => UNTYPED_KEYS.has(k));
}

/** Points the component refs at the `$defs` of the schema `addSchema` registers. */
function rewriteRefs(node: unknown): unknown {
  return JSON.parse(JSON.stringify(node).replaceAll(`"${COMPONENT_PREFIX}`, `"${DEFS_ID}#/$defs/`));
}

function newAjv(dateTime: 'upstream' | 'loose'): Ajv2020 {
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  addFormats(ajv);
  if (dateTime === 'upstream') {
    // 正規表現は桁の形しか見ないので、13 月のような有り得ない日時は ajv-formats の判定で落とす。
    // 判定は別の ajv で作る。同じ ajv で先に compile すると、書式が名前で覚えられて上書きが効かない
    const valid = newAjv('loose').compile({ type: 'string', format: 'date-time' });
    ajv.addFormat('date-time', { type: 'string', validate: (s: string) => UPSTREAM_DATE_TIME.test(s) && valid(s) });
  }
  // id と misskey:id は本家でも書式を決めていない印なので、文字列であることだけを見る
  for (const f of ['id', 'misskey:id', 'bytes', 'md5']) ajv.addFormat(f, true);
  ajv.addKeyword('example');
  return ajv;
}

/** Compiles the response schemas of an /api.json document. */
export function compileResponseSchemas(doc: OpenAPIDoc): ResponseSchemas {
  const elythiaComponents = elythiaOnlyComponents(doc);
  const defs: Record<string, unknown> = {};
  for (const [name, schema] of Object.entries(doc.components.schemas)) {
    if (!elythiaComponents.has(name)) {
      defs[name] = schema;
      continue;
    }
    defs[name] = closeSchema(schema, elythiaComponents);
    defs[`${name}${OPEN_SUFFIX}`] = closeSchema(schema, elythiaComponents, true);
  }
  const ajvs = { strict: newAjv('upstream'), loose: newAjv('loose') };
  for (const ajv of Object.values(ajvs)) ajv.addSchema({ $id: DEFS_ID, $defs: rewriteRefs(defs) });

  const elythiaEndpoints = Object.entries(doc.paths)
    .filter(([, item]) => item.post?.tags?.includes(ELYTHIA_TAG))
    .map(([path]) => path.slice(1));
  const cache = new Map<string, ExpectedResponse>();
  return {
    elythiaEndpoints,
    elythiaComponents,
    expected(endpoint) {
      const hit = cache.get(endpoint);
      if (hit != null) return hit;
      const op = doc.paths[`/${endpoint}`]?.post;
      if (op == null) throw new Error(`${endpoint} is not in /api.json`);
      if (!op.tags?.includes(ELYTHIA_TAG)) throw new Error(`${endpoint} is not an Elythia endpoint; only those are checked`);
      const res = op.responses ?? {};
      let exp: ExpectedResponse;
      if (res['200'] != null) {
        const raw = res['200'].content?.['application/json']?.schema;
        // 空の schema や欄の無い object は何でも通すので、型の無い 200 は照合しない (宣言の書き漏れとして落とす)
        if (!isObject(raw) || isUntyped(raw)) throw new Error(`${endpoint}: the 200 of /api.json has no schema`);
        const schema = rewriteRefs(closeSchema(raw, elythiaComponents)) as Schema;
        exp = { status: 200, schema, strict: ajvs.strict.compile(schema), loose: ajvs.loose.compile(schema) };
      } else if (res['204'] != null) {
        exp = { status: 204 };
      } else {
        throw new Error(`${endpoint}: /api.json declares neither 200 nor 204`);
      }
      cache.set(endpoint, exp);
      return exp;
    },
  };
}

let loaded: Promise<ResponseSchemas> | undefined;

/** Fetches /api.json once per worker and compiles it. */
export function responseSchemas(request: APIRequestContext): Promise<ResponseSchemas> {
  loaded ??= (async () => {
    const resp = await request.get(`${baseURL()}/api.json`, { failOnStatusCode: false });
    if (resp.status() !== 200) throw new Error(`GET /api.json: ${resp.status()}`);
    return compileResponseSchemas(await resp.json() as OpenAPIDoc);
  })();
  // 取り損ねた結果を使い回さない (次のテストで取り直す)
  loaded.catch(() => { loaded = undefined; });
  return loaded;
}

/** Formats ajv errors for an assertion message. */
export function describeErrors(errors: ErrorObject[] | null | undefined): string {
  return (errors ?? []).map((e) => `${e.instancePath || '/'} ${e.message}${e.keyword === 'unevaluatedProperties' ? ` (${String((e.params as { unevaluatedProperty?: string }).unevaluatedProperty)})` : ''}`).join('; ');
}

/** Options of {@link checkResponse}. */
export interface ExpectOptions {
  /**
   * The issue of a known bug that makes the dates of this response fail the
   * upstream format (`'#4091'`). The response must then fail only on that:
   * it must pass with the loose date-time of ajv-formats, and fail with the
   * upstream one. Once it passes, the check says to remove the mark.
   */
  knownBug?: string;
}

/**
 * Checks a response of an Elythia endpoint against the compiled /api.json: the
 * status the declaration gives (200 or 204), and the body against the
 * response type. Returns the parsed body (undefined for 204).
 */
export function checkResponse(schemas: ResponseSchemas, endpoint: string, status: number, text: string, opts: ExpectOptions = {}): unknown {
  const exp = schemas.expected(endpoint);
  expect(status, `${endpoint}: status ${status}, want ${exp.status}: ${text.slice(0, 500)}`).toBe(exp.status);
  if (exp.status === 204) {
    expect(text, `${endpoint}: 204 must have no body`).toBe('');
    expect(opts.knownBug, `${endpoint}: knownBug is for dates in a body; 204 has none`).toBeUndefined();
    return undefined;
  }
  const body: unknown = JSON.parse(text);
  if (opts.knownBug == null) {
    expect(exp.strict(body), `${endpoint}: ${describeErrors(exp.strict.errors)}`).toBe(true);
    return body;
  }
  // 既知の不具合は、日時の書式だけで落ちることを確かめる。ほかの食い違いまで受けると、壊れても緑になる
  expect(exp.loose(body), `${endpoint}: fails on more than the dates of ${opts.knownBug}: ${describeErrors(exp.loose.errors)}`).toBe(true);
  // loose と strict の違いは date-time の書式だけなので、loose で通って strict で落ちるなら、落ちたのは日時
  expect(exp.strict(body), `${endpoint}: ${opts.knownBug} が直ったように見える (日時が本家の書式で返った)。knownBug を外す`).toBe(false);
  return body;
}

/** Checks an API response against the /api.json of the server; see {@link checkResponse}. */
export async function expectApiResponse<T = unknown>(request: APIRequestContext, endpoint: string, resp: APIResponse, opts: ExpectOptions = {}): Promise<T> {
  return checkResponse(await responseSchemas(request), endpoint, resp.status(), await resp.text(), opts) as T;
}

/** Posts to an Elythia endpoint and checks the response; see {@link expectApiResponse}. */
export async function callChecked<T = unknown>(request: APIRequestContext, endpoint: string, body: Record<string, unknown>, opts: ExpectOptions = {}): Promise<T> {
  const resp = await request.post(`${baseURL()}/api/${endpoint}`, { data: body, failOnStatusCode: false });
  return expectApiResponse<T>(request, endpoint, resp, opts);
}
