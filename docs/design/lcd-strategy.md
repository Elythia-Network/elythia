# Playwright LCD → strict 化による drift detection strategy

**Status**: Active。spec は現在 299 ファイル (`tests/playwright/`)。以下 §5 の数値は
**Phase 1-4 完了時点のスナップショット**で、現状は [testing.md](../testing.md) を参照

---

## 1. 背景

mk-go は Misskey TS の drop-in 互換 backend を目指す。「同一 frontend が動く」「同一 API client が壊れない」ことを保証するため、両 backend (mk-go / Misskey TS) を **同一 spec** で並列実行して挙動差を回帰検出する仕組みが要る。

問題点:

- spec を strict に書く (= `expect(resp.status()).toBe(200)`) と、**最初から両 backend で挙動が一致している箇所だけ** しか書けない
- 一方で「両 backend で形が違うけど frontend は気にしない」軽微な drift (例: 200 vs 204) を理由に spec が flaky になると、本当に直すべき drift が雑音に埋もれる
- mk-go 単独で書けば mk-go の挙動を文書化するだけになり、回帰検出には弱い

## 2. 解決策: LCD → strict 化サイクル

**Lowest Common Denominator (LCD)** で両 backend pass する spec を先に書き、後で drift fix が入ったら strict に格上げする 5 段階フロー:

1. **spec 作成**: backend-agnostic に書く (= URL だけ切替で両 backend 動く)
2. **両 backend 実行**: 挙動が一致しない箇所を発見
3. **LCD 化**: `expect([200, 204]).toContain(resp.status())` のように両方許容 + コメントで drift 内容と原因を記録
4. **drift issue 起票**: 個別 issue として切る (例: #870 / #877 / #929 等)
5. **drift fix PR**: mk-go を upstream Misskey TS の挙動に揃える + spec の LCD を strict (`expect(resp.status()).toBe(200)`) に格上げ

```
   spec 書く
       ↓
  両 backend 実行
       ↓
   挙動差発見?
   /          \
  no           yes
   |            ↓
strict 維持   LCD で吸収 + drift issue
              ↓
          drift fix PR
              ↓
            strict 化
```

## 3. なぜこの strategy か

### 代替案: 厳密一致のみ許す

両 backend が完全一致する箇所だけ spec 化する。だが mk-go は元々 drift backlog があり、「最初から一致している部分」だけ書くと spec カバレッジが進まない。

### 代替案: mk-go 単体で spec 化

mk-go 既存挙動をそのまま検証。drift 検出能力ゼロ。drop-in 互換目標と整合しない。

### LCD → strict が良い理由

- spec カバレッジ拡大と drift 検出が **同時** に進む
- LCD コメントが drift backlog のソースになる (= grep で残 LCD を探せる)
- strict 化のタイミングで drift fix の前後を spec 上で記録 (= regression guard も自然に作れる)

## 4. LCD の書き方ガイドライン

```ts
// ❌ 単に LCD を書くだけは情報量ゼロ
expect([200, 204]).toContain(resp.status());

// ✅ コメントで drift 内容 / 原因 / 解消条件を記録
// upstream TS は paramDef で `on` 必須、mk-go は GORM Updates(map) で
// pq.StringArray ラップ無しの []string が NULL 化して 500 になる drift
// (#932 admin/system-webhook と同 class)。両 backend pass する payload が
// 存在しないため [200, 204, 500] LCD で吸収する。
expect([200, 204, 500]).toContain(resp.status());
```

drift 解消後は LCD と strict で diff が綺麗に出る (= `[200, 204, 500]` → `200`)、レビューで意図が伝わる。

## 5. 実績 (Phase 1-4 完了時点)

- 96 spec / 35 directory / 242 endpoint cover (= router.go 登録 448 endpoint の 54.3%)
- 40+ 件 drift を発見・修正 (#798 ~ #944)
- 残 LCD 数: ~68 (= mostly `[200, 204]` の 2xx ambiguity、benign)
- 両 backend で green を維持 (当時は nightly の matrix 実行。現在は PR トリガーで
  mk-go を 4 シャード、TS backend は upstream 追従時の `workflow_dispatch` のみ、#2291 / #2609)

## 6. 関連

- 親 tracker: #744 (Playwright Phase 1-4)
- drift fix list: [docs/api-compatibility.md](../api-compatibility.md) の「Playwright Phase 1-4 由来の drift backlog」section
- workflow: [docs/testing.md](../testing.md) の「Playwright e2e」section
- CI: `.github/workflows/playwright.yml` (PR トリガー、4 シャード)
