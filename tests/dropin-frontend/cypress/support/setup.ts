// Phase 14-2 (#387) 以降で複数 spec から共用する federation setup helper。
//
// 3 instance (A, B, C) 全てに alice / bob / charlie を作って互いに follow
// 済にするには AP handshake (resolve + Follow Accept) 完了を待つ必要がある。
// 各 spec が before で独立に呼べるよう idempotent に設計する。
//
// 一度 setup が完了した DB (= compose を down しない run) であれば、
// 次回以降は signin-flow だけで速く通過する。

import { api, createRootOrSignin, INSTANCES, retryUntil, waitForInstance } from './api';

export interface Principal {
  id: string;
  token: string;
  username: string;
  instance: keyof typeof INSTANCES;
}

export interface Trio {
  alice: Principal;
  bob: Principal;
  charlie: Principal;
}

// 3 instance が ready なのを待ち、alice/bob/charlie を作成/再利用する。
//
// cypress は spec ごとに新しい runner プロセスで起動するため memory 上の
// token を再利用できない。admin/accounts/create は root 作成時しか通らず
// 2 回目以降は signin-flow にフォールバックする。signin-flow は Misskey の
// rate limit に引っかかって 429 を返すため、6 spec × 3 instance = 18 回
// 連続で叩くと rate limit 窓内で溢れる。
//
// 対策: cypress plugin task (`tokenCache:*`) で spec 間共有する。plugin
// process は cypress run 全体で 1 つなので memory 上の変数が persistent。
const CACHE_KEY = 'trio';

export function setupTrio(): Cypress.Chainable<Trio> {
  waitForInstance(INSTANCES.a);
  waitForInstance(INSTANCES.b);
  waitForInstance(INSTANCES.c);

  return cy.task<Trio | null>('tokenCache:get', CACHE_KEY).then((cached) => {
    if (cached && cached.alice?.token && cached.bob?.token && cached.charlie?.token) {
      return cy.wrap(cached);
    }
    return freshSetupTrio();
  });
}

function freshSetupTrio(): Cypress.Chainable<Trio> {
  const trio = {} as Trio;
  return createRootOrSignin(INSTANCES.a, 'alice', 'password1234')
    .then((r) => {
      trio.alice = { ...r, username: 'alice', instance: 'a' };
    })
    .then(() => createRootOrSignin(INSTANCES.b, 'bob', 'password1234'))
    .then((r) => {
      trio.bob = { ...r, username: 'bob', instance: 'b' };
    })
    .then(() => createRootOrSignin(INSTANCES.c, 'charlie', 'password1234'))
    .then((r) => {
      trio.charlie = { ...r, username: 'charlie', instance: 'c' };
    })
    // meta.federation の既定値は 2026.7.0 で none。素の instance は連合しないので
    // 明示的に有効化する (upstream が 2025-08 の TweakDefaultFederationSettings で
    // all → none に変えた)。root token が要るので 3 人作った後に流す。
    .then(() => {
      [INSTANCES.a, INSTANCES.b, INSTANCES.c].forEach((inst) => {
        const principal =
          inst === INSTANCES.a ? trio.alice : inst === INSTANCES.b ? trio.bob : trio.charlie;
        api(inst, 'admin/update-meta', { i: principal.token, federation: 'all' });
      });
    })
    .then(() => cy.task('tokenCache:set', { key: CACHE_KEY, value: trio }))
    .then(() => cy.wrap(trio));
}

// source instance の viewer が target Principal (= remote user + own token) を
// AP resolve して follow する。既 follow は許容。AP Follow ack の反映
// (target 側 instance の followers list に source が載る) まで待つ。
export function followRemote(viewer: Principal, target: Principal): Cypress.Chainable {
  const viewerInst = INSTANCES[viewer.instance];
  const targetInst = INSTANCES[target.instance];

  return retryUntil(
    () =>
      api(viewerInst, 'users/show', {
        i: viewer.token,
        username: target.username,
        host: targetInst.domain,
      }),
    (resp) => resp.status === 200 && resp.body?.username === target.username,
  )
    .then((resp) => {
      const remoteId = resp.body.id;
      return api(viewerInst, 'following/create', {
        i: viewer.token,
        userId: remoteId,
      });
    })
    .then((followResp) => {
      if (followResp.status !== 204 && followResp.status !== 200) {
        const code = followResp.body?.error?.code;
        if (code !== 'ALREADY_FOLLOWING') {
          throw new Error(
            `follow failed: ${followResp.status} ${JSON.stringify(followResp.body)}`,
          );
        }
      }
    })
    .then(() =>
      // target 側 instance に follower として載るのを待つ (AP ack まで)。
      // users/followers は Misskey では認証必須なので target 自身の token を使う。
      retryUntil(
        () =>
          api(targetInst, 'users/followers', {
            i: target.token,
            userId: target.id,
          }),
        (resp) => {
          if (resp.status !== 200 || !Array.isArray(resp.body)) {
            return false;
          }
          return resp.body.some((f: Record<string, unknown>) => {
            const follower = (f.follower ?? f) as Record<string, unknown>;
            return (follower.host ?? follower.followerHost) === viewerInst.domain;
          });
        },
        { retries: 30, interval: 3_000 },
      ),
    );
}

// A→B, A→C, B→C の**片方向** follow を 3 本張る。alice が bob / charlie を
// follow、bob が charlie を follow する形。Phase 14-2 時点の spec は
// "follower が followee のノートを home で受け取る" 検証に使うため片方向で
// 十分。逆方向 (B→A, C→A, C→B) が必要になる場合 (例: bob が alice の
// followers-visibility note を見るテスト) は別関数で拡張する。
export function establishFederation(trio: Trio): Cypress.Chainable {
  return cy
    .then(() => followRemote(trio.alice, trio.bob))
    .then(() => followRemote(trio.alice, trio.charlie))
    .then(() => followRemote(trio.bob, trio.charlie));
}

// A↔B, A↔C, B↔C の 6 本 bidirectional follow を張る。establishFederation の
// 片方向 follow では足りない spec 用。
//
// 具体的には「A の alice が投稿し B / C 側で観測する」向きの検証で必須。
// 片方向版だと alice を follow している者がいないため配送先が存在せず、
// 到達 assertion は永久に成立せず、不達 assertion は常に pass する偽陽性に
// なる (federation_allowlist.cy.ts が実際にこれで 95 日間 red だった)。
export function establishMutualFederation(trio: Trio): Cypress.Chainable {
  return cy
    .then(() => followRemote(trio.alice, trio.bob))
    .then(() => followRemote(trio.bob, trio.alice))
    .then(() => followRemote(trio.alice, trio.charlie))
    .then(() => followRemote(trio.charlie, trio.alice))
    .then(() => followRemote(trio.bob, trio.charlie))
    .then(() => followRemote(trio.charlie, trio.bob));
}

// 一つのノート投稿 + 指定 instance の timeline に届くまで poll。
export function waitForNoteInTimeline(
  viewer: Principal,
  text: string,
  opts: { retries?: number; interval?: number } = {},
): Cypress.Chainable {
  const inst = INSTANCES[viewer.instance];
  return retryUntil(
    () =>
      api(inst, 'notes/timeline', {
        i: viewer.token,
        limit: 40,
      }),
    (resp) =>
      resp.status === 200 &&
      Array.isArray(resp.body) &&
      resp.body.some((n: Record<string, unknown>) => n.text === text),
    { retries: opts.retries ?? 30, interval: opts.interval ?? 3_000 },
  );
}
