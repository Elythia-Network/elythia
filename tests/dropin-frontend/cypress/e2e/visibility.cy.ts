// Phase 14-2 (#387) visibility 種別の federation 挙動検証。
//
// bob (on B) が alice (on A, フォロワー) 向けに public / home / followers /
// specified の 4 種 visibility ノートを投稿する。alice 側の home timeline と
// mentions に届くべきものだけが届くことを確認する。

import { api, INSTANCES } from '../support/api';
import { skipInSwap } from '../support/mode';
import { establishFederation, setupTrio, waitForNoteInTimeline, Trio } from '../support/setup';

describe('dropin-frontend visibility (Phase 14-2)', () => {
  let trio: Trio;

  before(() => {
    setupTrio().then((t) => {
      trio = t;
    });
    cy.then(() => establishFederation(trio));
  });

  it('public note from bob reaches alice home timeline', () => {
    const marker = `phase14-vis-public-${Date.now()}`;
    api(INSTANCES.b, 'notes/create', {
      i: trio.bob.token,
      text: marker,
      visibility: 'public',
    });
    waitForNoteInTimeline(trio.alice, marker);
  });

  it('home visibility note from bob reaches alice home timeline', () => {
    const marker = `phase14-vis-home-${Date.now()}`;
    api(INSTANCES.b, 'notes/create', {
      i: trio.bob.token,
      text: marker,
      visibility: 'home',
    });
    waitForNoteInTimeline(trio.alice, marker);
  });

  it('followers visibility note from bob reaches alice home timeline', () => {
    const marker = `phase14-vis-followers-${Date.now()}`;
    api(INSTANCES.b, 'notes/create', {
      i: trio.bob.token,
      text: marker,
      visibility: 'followers',
    });
    waitForNoteInTimeline(trio.alice, marker);
  });

  it('specified DM from bob arrives in alice mentions (and home, per TS baseline)', function () {
    // swap mode で mk-A が specified 向け inbound Note を受け取っても
    // mentions に現れない (#397)。#397 修正後に skip 解除。
    skipInSwap(this, 'swap で specified DM が mk-A の mentions に届かない (#397)');
    const marker = `phase14-vis-specified-${Date.now()}`;

    // bob から見た alice@a の remote id を resolve。api() helper で統一
    // (Devin #390-2 #6)。
    api(INSTANCES.b, 'users/show', {
      i: trio.bob.token,
      username: 'alice',
      host: INSTANCES.a.domain,
    }).then((resp) => {
      expect(resp.status, 'bob can resolve alice').to.eq(200);
      const remoteAliceId = resp.body.id;
      return api(INSTANCES.b, 'notes/create', {
        i: trio.bob.token,
        text: marker,
        visibility: 'specified',
        visibleUserIds: [remoteAliceId],
      });
    });

    // specified DM を mentions 経由で確認する。Misskey TS は
    // `notes/mentions` default で DM も含む (mk-go は visibility=specified
    // 指定が必要な別仕様)。baseline (all TS) では default query で届く。
    // Phase 14-3 で mk 差し替え時にここの挙動差が見えてくるはず。
    // なお下記 home timeline assertion で判明した通り TS 2025.2.1 は
    // specified が自分宛の場合 home にも表示するのが default 挙動。
    // `visibility: 'specified'` で絞り込んで問い合わせる。TS 本家は default
    // で specified も含むが mk-go は default で specified を除外する API
    // behavior なので、両モードで pass するよう明示指定する。
    cy.then(() => {
      const poll = (left: number): Cypress.Chainable => {
        return api(INSTANCES.a, 'notes/mentions', {
          i: trio.alice.token,
          limit: 40,
          visibility: 'specified',
        }).then((resp) => {
          if (
            resp.status === 200 &&
            Array.isArray(resp.body) &&
            resp.body.some((n: Record<string, unknown>) => n.text === marker)
          ) {
            return resp;
          }
          if (left <= 0) {
            throw new Error(`alice never saw specified DM "${marker}" in mentions`);
          }
          return cy.wait(3_000, { log: false }).then(() => poll(left - 1));
        });
      };
      poll(30);
    });

    // home timeline での挙動も確認する。Misskey TS 2025.2.1 の実挙動は
    // specified で自分が recipient の場合 home にも表示されるので、
    // baseline としてはそれを golden にする。Phase 14-3 で mk-A に差し替えた
    // ときにこの挙動が一致するかが互換性チェックとなる (もし TS の挙動が
    // 変わっていたら mk もそれに追従する必要がある)。
    cy.then(() => {
      api(INSTANCES.a, 'notes/timeline', {
        i: trio.alice.token,
        limit: 40,
      }).then((resp) => {
        expect(resp.status, 'home timeline fetch').to.eq(200);
        const presentInHome = (resp.body as Record<string, unknown>[]).some(
          (n) => n.text === marker,
        );
        expect(
          presentInHome,
          'specified DM appears in home timeline on TS baseline (per 2025.2.1 実挙動)',
        ).to.eq(true);
      });
    });
  });
});
