// Phase 14-2 follow-up (#536) federation allowlist / "specified" / "none"
// mode の挙動を 3 instance で検証する。
//
// admin/update-meta で A の federation mode を切り替えると:
//   - federation: "all" (default)        → 全 host に deliver / inbound 受け入れ
//   - federation: "specified" + [B]      → B だけ deliver、C への deliver は skip、C からの inbound も reject
//   - federation: "none"                 → 全 deliver / inbound stop
//
// Misskey TS は ApDeliverService / inbox handler で federation mode を見ている。
// mk-go は #536 で同等の enforcement を入れた。本 spec で baseline (3 TS) と
// swap (A=mk-go) の両方で挙動が一致することを担保する。

import { api, INSTANCES } from '../support/api';
import {
  Trio,
  establishMutualFederation,
  setupTrio,
  waitForNoteInTimeline,
} from '../support/setup';

// admin/update-meta は alice/bob/charlie が各々 instance の root として
// 作成されているので、自分の token で叩ける。federation field を意図した
// 状態に揃える小さな helper。
function setFederationMode(
  inst: typeof INSTANCES.a,
  rootToken: string,
  fields: { federation?: string; federationHosts?: string[] },
): Cypress.Chainable {
  return api(inst, 'admin/update-meta', {
    i: rootToken,
    ...fields,
  }).then((resp) => {
    expect(resp.status, `update-meta on ${inst.domain} status`).to.be.oneOf([200, 204]);
  });
}

// note が一定時間経っても target instance の timeline に到達しない (= deliver
// が skip されている / inbound が拒否されている) ことを担保する negative test
// 用 helper。federation queue の遅延を考慮して 30s 程度待つ。
function expectNoteNeverArrives(
  viewer: Trio['alice'],
  marker: string,
  windowMs = 30_000,
): Cypress.Chainable {
  const inst = INSTANCES[viewer.instance];
  const start = Date.now();
  const poll = (): Cypress.Chainable => {
    if (Date.now() - start >= windowMs) {
      return cy.wrap(null);
    }
    return api(inst, 'notes/timeline', {
      i: viewer.token,
      limit: 50,
    }).then((resp) => {
      if (resp.status === 200 && Array.isArray(resp.body)) {
        const found = (resp.body as Array<Record<string, unknown>>).some(
          (n) => n.text === marker,
        );
        if (found) {
          throw new Error(
            `unexpected note "${marker}" arrived at ${inst.domain} (federation policy should have blocked it)`,
          );
        }
      }
      return cy.wait(3_000, { log: false }).then(poll);
    });
  };
  return poll();
}

describe('dropin-frontend federation allowlist (#536)', () => {
  let trio: Trio;

  before(() => {
    setupTrio().then((t) => {
      trio = t;
    });
    // **双方向** follow を張る必要がある。この spec だけが「A の alice が
    // 投稿し、B / C 側で観測する」向きを検証するが、片方向版
    // (establishFederation) では alice を follow している者が誰もいないため
    // alice の public note には配送先が 1 つも存在しない。結果、
    // (a) 「B に届く」assertion は永久に成立せず、
    // (b) 「C に届かない」assertion は federation mode に関係なく常に pass
    //     する偽陽性になり、#536 の allowlist 実装を一切検証できていなかった。
    cy.then(() => establishMutualFederation(trio));
  });

  // テスト終了後に A の federation mode を必ず "all" に戻す。後続 spec が
  // この spec の状態を引きずらないようにするための housekeeping。
  afterEach(() => {
    cy.then(() =>
      setFederationMode(INSTANCES.a, trio.alice.token, {
        federation: 'all',
        federationHosts: [],
      }),
    );
  });

  it('federation: "specified" with [B] allows A↔B but blocks A↔C', () => {
    cy.then(() =>
      setFederationMode(INSTANCES.a, trio.alice.token, {
        federation: 'specified',
        federationHosts: [INSTANCES.b.domain],
      }),
    );

    const marker = `phase14-allowlist-${Date.now()}`;
    cy.then(() =>
      api(INSTANCES.a, 'notes/create', {
        i: trio.alice.token,
        text: marker,
        visibility: 'public',
      }).then((resp) => {
        expect(resp.status).to.eq(200);
      }),
    );

    // B (in allowlist) には deliver が届く
    cy.then(() => waitForNoteInTimeline(trio.bob, marker, { retries: 30 }));

    // C (not in allowlist) には届かない。上の assertion が通っている =
    // 「同条件で配送自体は機能している」ことの裏付けが取れているので、
    // ここでの不達は allowlist が効いた結果と言える。
    cy.then(() => expectNoteNeverArrives(trio.charlie, marker, 20_000));
  });

  it('federation: "none" stops outbound deliver to both B and C', () => {
    cy.then(() =>
      setFederationMode(INSTANCES.a, trio.alice.token, {
        federation: 'none',
      }),
    );

    const marker = `phase14-fed-none-${Date.now()}`;
    cy.then(() =>
      api(INSTANCES.a, 'notes/create', {
        i: trio.alice.token,
        text: marker,
        visibility: 'public',
      }).then((resp) => {
        expect(resp.status).to.eq(200);
      }),
    );

    cy.then(() => expectNoteNeverArrives(trio.bob, marker, 20_000));
    cy.then(() => expectNoteNeverArrives(trio.charlie, marker, 20_000));
  });
});
