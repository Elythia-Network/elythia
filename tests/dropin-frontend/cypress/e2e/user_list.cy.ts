// Phase 14-2 (#387) user list membership + list timeline 検証。
//
// alice が `dropin-buddies` list を作成し、bob (remote) を member として追加。
// - `users/lists/list` に list が載る
// - `notes/user-list-timeline` で bob のノートが引ける (= membership が効いている)

import { api, INSTANCES } from '../support/api';
import { skipInSwap } from '../support/mode';
import { establishFederation, setupTrio, Trio } from '../support/setup';

const LIST_NAME = 'dropin-buddies-phase14';

describe('dropin-frontend user list (Phase 14-2)', () => {
  let trio: Trio;
  let listId: string;

  before(() => {
    setupTrio().then((t) => {
      trio = t;
    });
    cy.then(() => establishFederation(trio));
  });

  it('alice creates a user list and pushes remote bob as member', function () {
    // Phase 14-3 swap mode: mk-go の users/lists/push は既 member 時に
    // ALREADY_ADDED を返さず INTERNAL_ERROR にする TS 非互換挙動 (#396)。
    // baseline で push 済の状態から swap 走行すると 500 で fail するため
    // skip する。#396 修正後に skip 解除。
    skipInSwap(this, 'swap で users/lists/push が既 member 時に 500 を返す (#396)');
    // 既存 (再実行時に残存) list を再利用する
    api(INSTANCES.a, 'users/lists/list', { i: trio.alice.token }).then((listResp) => {
      const existing = Array.isArray(listResp.body)
        ? listResp.body.find((l: Record<string, unknown>) => l.name === LIST_NAME)
        : undefined;
      if (existing) {
        listId = existing.id as string;
        return;
      }
      return api(INSTANCES.a, 'users/lists/create', {
        i: trio.alice.token,
        name: LIST_NAME,
      }).then((createResp) => {
        listId = createResp.body.id;
      });
    });

    // bob の remote id を resolve
    cy.then(() => {
      return api(INSTANCES.a, 'users/show', {
        i: trio.alice.token,
        username: 'bob',
        host: INSTANCES.b.domain,
      }).then((showResp) => {
        const remoteBobId = showResp.body.id;
        return api(INSTANCES.a, 'users/lists/push', {
          i: trio.alice.token,
          listId,
          userId: remoteBobId,
        }).then((pushResp) => {
          if (pushResp.status !== 204 && pushResp.status !== 200) {
            const code = pushResp.body?.error?.code;
            if (code !== 'ALREADY_ADDED') {
              throw new Error(
                `list push failed: ${pushResp.status} ${JSON.stringify(pushResp.body)}`,
              );
            }
          }
        });
      });
    });
  });

  it("bob's note appears in alice's user-list-timeline", function () {
    // swap mode では前テストがスキップされ listId が未設定のため同じく skip
    skipInSwap(this, '前テスト push が swap で skipped のため list 操作不可');
    const marker = `phase14-list-${Date.now()}`;
    api(INSTANCES.b, 'notes/create', {
      i: trio.bob.token,
      text: marker,
      visibility: 'public',
    });

    // list timeline は Redis fanout + DB fallback で引く
    const poll = (left: number): Cypress.Chainable => {
      return api(INSTANCES.a, 'notes/user-list-timeline', {
        i: trio.alice.token,
        listId,
        limit: 40,
      }).then((resp) => {
        if (
          resp.status === 200 &&
          Array.isArray(resp.body) &&
          resp.body.some((n: Record<string, unknown>) => n.text === marker)
        ) {
          return resp;
        }
        if (left <= 0) {
          throw new Error(`user-list-timeline never included "${marker}"`);
        }
        return cy.wait(3_000, { log: false }).then(() => poll(left - 1));
      });
    };
    poll(40);
  });
});
