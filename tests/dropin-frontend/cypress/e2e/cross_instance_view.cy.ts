// Phase 14-2 (#387) cross-instance view consistency.
//
// charlie が 1 つ public note を投稿して、A と B それぞれの cypress セッション
// から観測した note (text / user.username / user.host) が一致することを確認。
// federation delivery の整合性が取れているか = 同じ原本が両サイドで見えるか。
//
// 観測先は `notes/timeline` (home 経由) のみ。A と B の home それぞれに
// charlie の note が届いて同一内容に見えれば十分。

import { api, INSTANCES } from '../support/api';
import { establishFederation, setupTrio, waitForNoteInTimeline, Trio } from '../support/setup';

describe('dropin-frontend cross-instance view (Phase 14-2)', () => {
  let trio: Trio;

  before(() => {
    setupTrio().then((t) => {
      trio = t;
    });
    cy.then(() => establishFederation(trio));
  });

  it('charlie note is observed identically on A and B via federation', () => {
    const marker = `phase14-cross-${Date.now()}`;

    api(INSTANCES.c, 'notes/create', {
      i: trio.charlie.token,
      text: marker,
      visibility: 'public',
    });

    // A と B 両方に届くまで待つ
    cy.then(() => waitForNoteInTimeline(trio.alice, marker, { retries: 40 }));
    cy.then(() => waitForNoteInTimeline(trio.bob, marker, { retries: 40 }));

    // A 側で見える note
    let aSide: Record<string, unknown> | undefined;
    cy.then(() =>
      api(INSTANCES.a, 'notes/timeline', {
        i: trio.alice.token,
        limit: 40,
      }).then((resp) => {
        aSide = (resp.body as Record<string, unknown>[]).find((n) => n.text === marker);
        expect(aSide, 'note visible on instance A').to.exist;
      }),
    );

    // B 側で見える note
    let bSide: Record<string, unknown> | undefined;
    cy.then(() =>
      api(INSTANCES.b, 'notes/timeline', {
        i: trio.bob.token,
        limit: 40,
      }).then((resp) => {
        bSide = (resp.body as Record<string, unknown>[]).find((n) => n.text === marker);
        expect(bSide, 'note visible on instance B').to.exist;
      }),
    );

    // author と text が一致する
    cy.then(() => {
      expect(aSide!.text).to.eq(bSide!.text);
      const aUser = aSide!.user as Record<string, unknown>;
      const bUser = bSide!.user as Record<string, unknown>;
      expect(aUser.username).to.eq('charlie');
      expect(bUser.username).to.eq('charlie');
      expect(aUser.host).to.eq(INSTANCES.c.domain);
      expect(bUser.host).to.eq(INSTANCES.c.domain);
    });
  });
});
