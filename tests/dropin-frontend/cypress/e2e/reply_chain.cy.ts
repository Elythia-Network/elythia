// Phase 14-2 (#387) reply chain の federation 連鎖検証。
//
// charlie 起点の note に bob が reply して、charlie の inbox に届くまでを
// 検証する 1 hop 版。3 instance bidirectional federation を使った 2 hop 版
// (charlie → alice 行き fed + bob → alice 行き fed の両方を timeline poll で
// 待つ) は Phase 14-2 当初の実装だが、複数 spec 同時走行の queue back-
// pressure で alice 側 timeline poll が 120 秒 retry 内に完了せず flaky だった
// (#389)。AP の reply 配送はそもそも replyTo の owner (= charlie) に対する
// 直接デリバリなので、charlie の notes/children に到達することを観測すれば
// reply chain の federation 整合性は十分担保できる。alice 経由の検証は
// 他 spec (cross_instance_view 等) でカバーされている。

import { api, INSTANCES, retryUntil } from '../support/api';
import { establishFederation, setupTrio, Trio } from '../support/setup';

describe('dropin-frontend reply chain (Phase 14-2, #389)', () => {
  let trio: Trio;

  before(() => {
    setupTrio().then((t) => {
      trio = t;
    });
    cy.then(() => establishFederation(trio));
  });

  it('bob reply to charlie note federates back to charlie', () => {
    const tag = Date.now().toString();
    const originalText = `phase14-chain-origin-${tag}`;
    const bobReplyText = `phase14-chain-bob-${tag}`;

    let originalId = '';

    // charlie が起点 note を投稿。alice 側への federation は本テストの関心外
    // なので poll しない (#389 の flaky 原因の 1 つ)。
    api(INSTANCES.c, 'notes/create', {
      i: trio.charlie.token,
      text: originalText,
      visibility: 'public',
    }).then((resp) => {
      originalId = resp.body.createdNote.id;
    });

    // bob が AP resolve で charlie の note を取得。AP fetch は同期 HTTP
    // なので queue back-pressure の影響を受けない。
    cy.then(() =>
      api(INSTANCES.b, 'ap/show', {
        i: trio.bob.token,
        uri: `${INSTANCES.c.url}/notes/${originalId}`,
      }).then((apResp) => {
        const bobSideOriginalId = apResp.body.object.id;
        return api(INSTANCES.b, 'notes/create', {
          i: trio.bob.token,
          text: bobReplyText,
          replyId: bobSideOriginalId,
          visibility: 'public',
        }).then((r) => {
          expect(r.status).to.eq(200);
        });
      }),
    );

    // bob の reply は AP 上 charlie 宛 (replyTo の owner) に直接配送される。
    // charlie の notes/children に bob のテキストが現れるまで poll する。
    // alice 側 timeline は経由しないので 1 federation hop で確定する。
    cy.then(() =>
      retryUntil(
        () =>
          api(INSTANCES.c, 'notes/children', {
            i: trio.charlie.token,
            noteId: originalId,
            limit: 30,
          }),
        (resp) =>
          resp.status === 200 &&
          Array.isArray(resp.body) &&
          resp.body.some((n: Record<string, unknown>) => n.text === bobReplyText),
        { retries: 40, interval: 3_000 },
      ),
    );
  });
});
