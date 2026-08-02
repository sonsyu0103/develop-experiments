# ADR 0003: 未決事項の一覧

- ステータス: 進行中
- 日付: 2026-08-02 / 更新: 2026-08-03

## 解決済み

### ✅ 1. フロントエンドに `app/` が 2 つあった

`apps/next-app/src/` を削除し、`app/` に一本化した。

`src/app/threads/page.tsx` は Next.js の解決規則 (`app/` と `src/app/` が
両方あると `src/` 側が無視される) によりデッドコードであり、
かつ 3 箇所が壊れていた (`'use client'` 欠落、レスポンス形の誤り、
フィールド名の誤り)。
手書きの型定義 `src/types/api.d.ts` も API と一致していなかったため削除した。

`schema.d.ts` が仕様書から自動生成される以上、手書きの型を残す理由がない。

### ✅ 2. `api/openapi/swagger.yaml` が別系統で残っていた

削除した。同じ場所 (`api/openapi.yaml`) に、
**手書きの唯一の正**としての仕様書を置き直した ([ADR 0002](0002-openapi-direction.md))。

### ✅ 3. ESLint が実質的に動いていなかった

正式に導入した。

- `eslint` + `eslint-config-next` を devDependencies に追加
- フラット設定 `apps/next-app/eslint.config.mjs` を作成
- Next.js 16 で `next lint` は削除されたため、`eslint .` を直接呼ぶ形に変更
- 生成物 (`schema.d.ts`) は検査対象から除外
- `no-explicit-any` をエラーに引き上げ (型が生成されている以上 any は不要)

**ESLint は 9 系に固定している。**
`eslint-config-next` の peerDependencies は `>=9.0.0` だが、
同梱の `eslint-plugin-react@^7.37` が ESLint 10 で削除された
`context.getFilename()` を呼ぶため、10 系では起動時にクラッシュする。

### ✅ 4. Next.js が 13.4.7 で止まっていた

以下に更新した。

| | 変更前 | 変更後 |
| --- | --- | --- |
| next | `latest` (実体 13.4.7) | `^16.2.12` |
| react / react-dom | `latest` (実体 18.2.0) | `^19.2.8` |
| typescript | `latest` | `^5.9.3` |
| eslint | (なし) | `^9.39.5` |

`"latest"` 指定をすべて明示的なレンジに置き換えた。
`"latest"` は「バージョン指定」ではなく dist-tag であり、
lock を作り直した瞬間に別バージョンが降ってくるため再現性がない。

TypeScript は 7 系ではなく 5.9 系に留めた。
`eslint-config-next` が検証している組み合わせから外れるリスクを避けるため。

Next 16 のビルド時に `tsconfig.json` が自動更新され、
`jsx` が `react-jsx` (automatic runtime) になった。
これに伴い不要になった `import React` を削除している。

### ✅ 5. OpenAPI スキーマの向き

yaml を正とする方式に切り替えた。詳細は [ADR 0002](0002-openapi-direction.md)。

---

## 未決

### 6. 認証

現状は認証なし (誰でもスレッド作成・コメント投稿ができる)。

ポートフォリオとしてどう扱うか:

- **入れない** — README に「認証は範囲外」と明記する。
  主題が並行制御である以上、これは正当なスコープ設定
- **入れる** — 題材としては手堅いが、主題から外れて時間を取られる

- [ ] どちらにするか

### 7. コメント削除 API

リポジトリ層に論理削除 (`SoftDelete`) を実装済みだが、
HTTP エンドポイントとしては公開していない。

公開する場合、認証なしでは誰でも他人のコメントを消せてしまうため、
項目 6 とセットで決める必要がある。

- [ ] 公開するか

### 8. 検証の値が 3 箇所に散っている

`title` の最大長 200 が、仕様書 / ドメイン定数 / DB の CHECK 制約の
3 箇所に書かれている。役割が違うため意図的な重複だが、将来ずれる危険はある。

現状はコメントで対応関係を明記して運用している。

- [ ] 仕様書から Go の定数を生成するなどして一元化するか
      (`x-go-` 拡張を使う手はあるが、複雑さに見合うかは要検討)

## 決定済みの API 仕様

| 項目 | 決定 |
| --- | --- |
| ページネーション | キーセット (cursor) 方式。`?cursor=&size=`、既定 20 件、上限 100 件 |
| エラー形式 | `{"error": {"code": "...", "message": "..."}}` |
| CORS | 許可リスト方式。既定は `http://localhost:3000` |
| スレッド作成 | `POST /threads` を実装済み |

## Phase 2 に残した作業

コメント投稿 (`POST /threads/{threadId}/comments`) は
**基本的な実装のみ**を済ませてある。以下は Phase 2 の作業として残っている。

- SERIALIZABLE 分離レベルでのトランザクション実装
- 直列化失敗 (SQLSTATE 40001) の指数バックオフつきリトライ
  (`postgres.IsRetryable` が起点)
- 悲観ロック版 (`SELECT ... FOR UPDATE`) との比較実装
- 両者のスループット比較ベンチマーク
