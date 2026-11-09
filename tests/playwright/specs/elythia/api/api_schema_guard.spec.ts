/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 応答の照合の helper (fixtures/api_schema.ts、#4093) が、照合したつもりで何も見ていない
// 状態にならないことを確かめる。前半は小さな /api.json を組み立てて規則を確かめ、後半は
// サーバーの /api.json を読んで、独自の API がすべて照合の対象になることを確かめる。

import { expect, test } from '@playwright/test';
import { callChecked, checkResponse, compileResponseSchemas, knownDateBug, responseSchemas } from '../../../fixtures/api_schema';
import { isTsBackend } from '../../../fixtures/backend';

const ELYTHIA = { tags: ['Elythia'] };
const ok = (schema: Record<string, unknown>) => ({ '200': { content: { 'application/json': { schema } } } });

// 本家の operation が引く Up と、独自の operation だけが引く Own・Wrap・Outer、Outer からだけ引かれる
// Inner を持つ /api.json
const doc = {
  paths: {
    '/upstream': { post: { tags: ['notes'], responses: ok({ $ref: '#/components/schemas/Up' }) } },
    '/own': { post: { ...ELYTHIA, responses: ok({ $ref: '#/components/schemas/Own' }) } },
    '/own/list': { post: { ...ELYTHIA, responses: ok({ type: 'array', items: { $ref: '#/components/schemas/Own' } }) } },
    '/own/wrap': { post: { ...ELYTHIA, responses: ok({ $ref: '#/components/schemas/Wrap' }) } },
    '/own/with-up': {
      post: {
        ...ELYTHIA,
        responses: ok({ type: 'object', allOf: [{ $ref: '#/components/schemas/Up' }, { type: 'object', properties: { token: { type: 'string' } }, required: ['token'] }] }),
      },
    },
    '/own/inline': {
      post: {
        ...ELYTHIA,
        responses: ok({ type: 'object', properties: { user: { $ref: '#/components/schemas/Up' }, at: { type: ['string', 'null'], format: 'date-time' } }, required: ['user', 'at'] }),
      },
    },
    '/own/deep': { post: { ...ELYTHIA, responses: ok({ $ref: '#/components/schemas/Outer' }) } },
    '/own/items': { post: { ...ELYTHIA, responses: ok({ type: 'array', items: { type: 'object', properties: { n: { type: 'integer' } }, required: ['n'] } }) } },
    '/own/either': { post: { ...ELYTHIA, responses: ok({ oneOf: [{ type: 'object', properties: { a: { type: 'string' } }, required: ['a'] }, { type: 'null' }] }) } },
    '/own/empty': { post: { ...ELYTHIA, responses: { '204': { description: 'OK (without any results)' } } } },
    '/own/untyped': { post: { ...ELYTHIA, responses: ok({}) } },
    '/own/bare-object': { post: { ...ELYTHIA, responses: ok({ type: 'object' }) } },
    '/own/bare-array': { post: { ...ELYTHIA, responses: ok({ type: 'array', description: 'list' }) } },
    '/own/count': { post: { ...ELYTHIA, responses: ok({ type: 'integer' }) } },
    '/own/none': { post: { ...ELYTHIA } },
  },
  components: {
    schemas: {
      Up: { type: 'object', properties: { id: { type: 'string', format: 'id' } }, required: ['id'] },
      Own: { type: 'object', properties: { id: { type: 'string', format: 'id' }, createdAt: { type: 'string', format: 'date-time' } }, required: ['id', 'createdAt'] },
      Outer: { type: 'object', properties: { inner: { $ref: '#/components/schemas/Inner' } }, required: ['inner'] },
      Inner: { type: 'object', properties: { n: { type: 'integer' } }, required: ['n'] },
      Wrap: { type: 'object', allOf: [{ $ref: '#/components/schemas/Own' }, { type: 'object', properties: { extra: { type: 'integer' } }, required: ['extra'] }] },
    },
  },
};

const DATE = '2026-11-09T01:02:03.456Z';
const GO_DATE = '2026-11-09T01:02:03.456789Z';
const own = { id: 'a1', createdAt: DATE };

test.describe('the response check of the Elythia endpoints', () => {
  const schemas = compileResponseSchemas(doc);
  const check = (endpoint: string, body: unknown, opts = {}) => checkResponse(schemas, endpoint, 200, JSON.stringify(body), opts);

  test('only the components the Elythia endpoints alone refer to are Elythia types', () => {
    // Inner は独自の operation から直接は引かれず、Outer を通してだけ引かれる
    expect([...schemas.elythiaComponents].sort()).toEqual(['Inner', 'Outer', 'Own', 'Wrap']);
    expect(schemas.elythiaEndpoints).toContain('own');
    expect(schemas.elythiaEndpoints).not.toContain('upstream');
  });

  test('a body of the declared type passes', () => {
    expect(check('own', own)).toEqual(own);
    check('own/list', [own, own]);
    check('own/wrap', { ...own, extra: 1 });
    check('own/with-up', { id: 'u1', token: 't', addedByElythia: true });
    check('own/inline', { user: { id: 'u1', addedByElythia: 1 }, at: null });
    check('own/deep', { inner: { n: 1 } });
    check('own/items', [{ n: 1 }]);
    check('own/either', { a: 'x' });
    check('own/either', null);
  });

  test('a missing field, a wrong type and the Go date format fail', () => {
    expect(() => check('own', { id: 'a1' })).toThrow(/must have required property 'createdAt'/);
    expect(() => check('own', { ...own, id: 1 })).toThrow(/\/id must be string/);
    expect(() => check('own', { ...own, createdAt: GO_DATE })).toThrow(/\/createdAt must match format "date-time"/);
    expect(() => check('own', { ...own, createdAt: '2026-11-09T01:02:03.456+09:00' })).toThrow(/must match format "date-time"/);
    // 桁の形は本家の書式でも、有り得ない日時は落とす
    expect(() => check('own', { ...own, createdAt: '2026-13-45T99:99:99.000Z' })).toThrow(/must match format "date-time"/);
    expect(() => check('own/inline', { user: { id: 'u1' }, at: GO_DATE })).toThrow(/\/at must match format "date-time"/);
  });

  test('a field an Elythia type does not declare fails, also through allOf and arrays', () => {
    expect(() => check('own', { ...own, stray: 1 })).toThrow(/must NOT have unevaluated properties \(stray\)/);
    expect(() => check('own/list', [own, { ...own, stray: 1 }])).toThrow(/\/1 must NOT have unevaluated properties/);
    expect(() => check('own/wrap', { ...own, extra: 1, stray: 1 })).toThrow(/unevaluated properties \(stray\)/);
    expect(() => check('own/inline', { user: { id: 'u1' }, at: null, stray: 1 })).toThrow(/unevaluated properties \(stray\)/);
    // 入れ子の component、配列の要素と oneOf の枝の inline の object も閉じる
    expect(() => check('own/deep', { inner: { n: 1, stray: 1 } })).toThrow(/\/inner must NOT have unevaluated properties \(stray\)/);
    expect(() => check('own/items', [{ n: 1, stray: 1 }])).toThrow(/\/0 must NOT have unevaluated properties \(stray\)/);
    expect(() => check('own/either', { a: 'x', stray: 1 })).toThrow(/unevaluated properties \(stray\)/);
  });

  test('the status must be the declared one, and 204 has no body', () => {
    expect(() => checkResponse(schemas, 'own', 204, '')).toThrow(/own: status 204, want 200/);
    expect(() => checkResponse(schemas, 'own/empty', 200, '{}')).toThrow(/own\/empty: status 200, want 204/);
    expect(() => checkResponse(schemas, 'own/empty', 204, '{}')).toThrow(/204 must have no body/);
    expect(checkResponse(schemas, 'own/empty', 204, '')).toBeUndefined();
  });

  test('an endpoint without a response type, an upstream endpoint and an unknown one are not checked', () => {
    expect(() => schemas.expected('own/untyped')).toThrow(/has no schema/);
    expect(() => schemas.expected('own/bare-object')).toThrow(/has no schema/);
    expect(() => schemas.expected('own/bare-array')).toThrow(/has no schema/);
    // 値が数や文字列の宣言は、型を決めている
    expect(check('own/count', 3)).toBe(3);
    expect(() => check('own/count', 'x')).toThrow(/must be integer/);
    expect(() => schemas.expected('own/none')).toThrow(/neither 200 nor 204/);
    expect(() => schemas.expected('upstream')).toThrow(/not an Elythia endpoint/);
    expect(() => schemas.expected('nope')).toThrow(/not in \/api.json/);
  });

  test('a known bug passes only while the dates alone fail', () => {
    // 日時だけが Go の書式で落ちる間は、既知の不具合として通す
    check('own', { ...own, createdAt: GO_DATE }, { knownBug: '#4091' });
    // 直ったら、印を外させる
    expect(() => check('own', own, { knownBug: '#4091' })).toThrow(/#4091 が直ったように見える/);
    // 日時のほかにも食い違いがあれば落とす
    expect(() => check('own', { ...own, createdAt: GO_DATE, stray: 1 }, { knownBug: '#4091' })).toThrow(/fails on more than the dates of #4091/);
    expect(() => checkResponse(schemas, 'own/empty', 204, '', { knownBug: '#4091' })).toThrow(/204 has none/);
  });

  test('knownDateBug marks a body as the known bug only while it has a date in the Go format', () => {
    expect(knownDateBug(JSON.stringify({ ...own, createdAt: GO_DATE }), '#4091')).toEqual({ knownBug: '#4091' });
    expect(knownDateBug(JSON.stringify([own, { ...own, createdAt: GO_DATE }]), '#4091')).toEqual({ knownBug: '#4091' });
    // 日時が無いか、どれも本家の書式なら、印を付けない (付けると「直ったように見える」で落ちる)
    expect(knownDateBug(JSON.stringify({ rows: [] }), '#4091')).toEqual({});
    expect(knownDateBug(JSON.stringify(own), '#4091')).toEqual({});
    expect(knownDateBug(JSON.stringify({ id: 'aTb', at: null }), '#4091')).toEqual({});
    // 照合と組み合わせると、どちらの体も通る
    for (const body of [own, { ...own, createdAt: GO_DATE }]) {
      const text = JSON.stringify(body);
      expect(checkResponse(schemas, 'own', 200, text, knownDateBug(text, '#4091'))).toEqual(body);
    }
  });
});

test.describe('the response types of the server', () => {
  test.skip(isTsBackend, '/api.json の Elythia の operation は Elythia にしか無い');

  // 宣言の無いものは、ここに番号と一緒に挙げる。宣言が付いたら外す
  const PENDING: Record<string, string> = {
    'drive/files/create-chunked/append': '#4090',
  };

  test('every Elythia endpoint has a response type to check against', async ({ request }) => {
    const schemas = await responseSchemas(request);
    // 抽出が空振りしても緑にならないよう、実在するものを名指しで要求する
    expect(schemas.elythiaEndpoints.length).toBeGreaterThanOrEqual(87);
    expect(schemas.elythiaEndpoints).toEqual(expect.arrayContaining(['bubble-game/versus/show', 'chat/messages', 'admin/backup/list', 'signup-application/apply']));
    expect([...schemas.elythiaComponents]).toEqual(expect.arrayContaining(['BubbleVersusMatch', 'RelatedEmojiApplication', 'BubbleVersusResultSummary']));
    expect([...schemas.elythiaComponents]).not.toContain('UserLite');
    const failed: string[] = [];
    for (const endpoint of schemas.elythiaEndpoints) {
      let error: string | undefined;
      try {
        schemas.expected(endpoint);
      } catch (e) {
        error = (e as Error).message;
      }
      const pending = PENDING[endpoint];
      if (pending == null && error != null) failed.push(error);
      if (pending != null && error == null) failed.push(`${endpoint}: has a response type now; remove it from PENDING (${pending})`);
    }
    for (const endpoint of Object.keys(PENDING)) {
      if (!schemas.elythiaEndpoints.includes(endpoint)) failed.push(`${endpoint}: not in /api.json; remove it from PENDING`);
    }
    expect(failed).toEqual([]);
  });

  test('callChecked checks the response it gets', async ({ request }) => {
    // 認証の無い呼び出しは 401 で返る。照合を通さずに body を返す helper なら、ここで落ちない
    await expect(callChecked(request, 'bubble-game/versus/invitations', {})).rejects.toThrow(/bubble-game\/versus\/invitations: status 401, want 200/);
  });
});
