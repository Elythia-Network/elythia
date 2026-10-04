// Phase 14-1 (#381) cypress baseline spec。
//
// 3 インスタンス (A, B, C) の Misskey TS が federation で動いていることを
// cypress runtime から API レベルで確認する。Phase 14-2 でブラウザ UI 操作
// の spec を追加する。

import { INSTANCES, api, createRootOrSignin, retryUntil, waitForInstance } from '../support/api';

describe('dropin-frontend smoke (Phase 14-1)', () => {
  let aliceToken: string;
  let bobId: string;
  let bobToken: string;
  let charlieToken: string;

  before(() => {
    // 3 instance 起動完了待ち。docker compose の healthcheck で既に ready な
    // はずだが cypress の起動タイミング次第で federation 初期化中のことも
    // ある。
    waitForInstance(INSTANCES.a);
    waitForInstance(INSTANCES.b);
    waitForInstance(INSTANCES.c);

    createRootOrSignin(INSTANCES.a, 'alice', 'password1234').then((r) => {
      aliceToken = r.token;
    });
    createRootOrSignin(INSTANCES.b, 'bob', 'password1234').then((r) => {
      bobId = r.id;
      bobToken = r.token;
    });
    createRootOrSignin(INSTANCES.c, 'charlie', 'password1234').then((r) => {
      charlieToken = r.token;
    });
  });

  it('each instance responds to /api/ping', () => {
    // Misskey TS は `{pong: <timestamp>}`、mk-go は `{pong: true}` を返す。
    // Phase 14-3 で mk 差し替えた時も通るよう truthy 判定のみ。
    for (const inst of Object.values(INSTANCES)) {
      api(inst, 'ping').then((resp) => {
        expect(resp.status).to.eq(200);
        expect(resp.body.pong).to.be.ok;
      });
    }
  });

  it('instance A can webfinger its own root user', () => {
    cy.request({
      url: `${INSTANCES.a.url}/.well-known/webfinger?resource=acct:alice@${INSTANCES.a.domain}`,
    }).then((resp) => {
      expect(resp.status).to.eq(200);
      expect(resp.body.subject).to.eq(`acct:alice@${INSTANCES.a.domain}`);
    });
  });

  it('alice@a can resolve bob@b and follow via federation', () => {
    // alice が bob@b を AP resolve するまで待つ
    retryUntil(
      () =>
        api(INSTANCES.a, 'users/show', {
          i: aliceToken,
          username: 'bob',
          host: INSTANCES.b.domain,
        }),
      (resp) => resp.status === 200 && resp.body?.username === 'bob',
    ).then((resp) => {
      const remoteBobId = resp.body.id;

      // follow 実行 (既に followed ならエラーは無視)
      api(INSTANCES.a, 'following/create', {
        i: aliceToken,
        userId: remoteBobId,
      }).then((followResp) => {
        if (followResp.status !== 204 && followResp.status !== 200) {
          const code = followResp.body?.error?.code;
          if (code !== 'ALREADY_FOLLOWING') {
            throw new Error(
              `follow failed: ${followResp.status} ${JSON.stringify(followResp.body)}`,
            );
          }
        }
      });
    });

    // bob 側に follower として alice が現れるまで retry
    retryUntil(
      () =>
        api(INSTANCES.b, 'users/followers', {
          i: bobToken,
          userId: bobId,
        }),
      (resp) => {
        if (resp.status !== 200 || !Array.isArray(resp.body)) {
          return false;
        }
        return resp.body.some((f: any) => {
          const follower = f.follower ?? f;
          return (follower.host ?? follower.followerHost) === INSTANCES.a.domain;
        });
      },
    );
  });

  it("alice@a receives charlie@c's public note via federation", () => {
    let charlieId: string;

    // charlie@c を alice が AP resolve + follow
    retryUntil(
      () =>
        api(INSTANCES.a, 'users/show', {
          i: aliceToken,
          username: 'charlie',
          host: INSTANCES.c.domain,
        }),
      (resp) => resp.status === 200 && resp.body?.username === 'charlie',
    ).then((resp) => {
      const remoteCharlieId = resp.body.id;
      api(INSTANCES.a, 'following/create', {
        i: aliceToken,
        userId: remoteCharlieId,
      }).then((followResp) => {
        // 既 follow は許容、それ以外の 4xx/5xx は即 fail させて late timeout と
        // confusing error message を避ける (Devin #385-2 #5)。
        if (followResp.status !== 204 && followResp.status !== 200) {
          const code = followResp.body?.error?.code;
          if (code !== 'ALREADY_FOLLOWING') {
            throw new Error(
              `charlie follow failed: ${followResp.status} ${JSON.stringify(followResp.body)}`,
            );
          }
        }
      });
    });

    // C 側が alice を follower として認識するまで待つ。AP Follow ack が非同期
    // で走るので `following/create` の 204 返却直後だと配信対象に入らない。
    cy.request({ url: `${INSTANCES.c.url}/api/i`, method: 'POST', body: { i: charlieToken } }).then((me) => {
      charlieId = me.body.id;

      retryUntil(
        () =>
          api(INSTANCES.c, 'users/followers', { i: charlieToken, userId: charlieId }),
        (resp) => {
          if (resp.status !== 200 || !Array.isArray(resp.body)) {
            return false;
          }
          return resp.body.some((f: any) => {
            const follower = f.follower ?? f;
            return (follower.host ?? follower.followerHost) === INSTANCES.a.domain;
          });
        },
        { retries: 30, interval: 3_000 },
      );
    });

    // charlie がノートを投稿
    const marker = `phase14-smoke-${Date.now()}`;
    cy.then(() => {
      api(INSTANCES.c, 'notes/create', {
        i: charlieToken,
        text: marker,
        visibility: 'public',
      }).then((resp) => {
        expect(resp.status).to.eq(200);
        expect(resp.body.createdNote.text).to.eq(marker);
      });
    });

    // alice の home timeline に届くまで poll (federation 配信 + queue 処理)
    cy.then(() => {
      retryUntil(
        () =>
          api(INSTANCES.a, 'notes/timeline', {
            i: aliceToken,
            limit: 40,
          }),
        (resp) =>
          resp.status === 200 &&
          Array.isArray(resp.body) &&
          resp.body.some((n: any) => n.text === marker),
        { retries: 40, interval: 3_000 },
      );
    });
  });
});
