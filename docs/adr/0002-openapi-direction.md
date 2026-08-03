# ADR 0002: OpenAPI スキーマの「正」をどちらに置くか

- ステータス: **採用 (選択肢 C)**
- 日付: 2026-08-02 / 決定: 2026-08-03

## 背景

要件では「OpenAPI スキーマを起点とした駆動開発」を掲げていた。
しかし当初の実装は逆向きであり、しかも自動生成になっていなかった。

**変更前:**

```
Go のハンドラ実装
      │  (人間が手で同期させる)  ← ここが手作業
      ▼
cmd/generate-docs/main.go   ← openapi3.T を手で組み立てていた
      │  go run
      ▼
apps/go-api/openapi.yaml
      │  openapi-typescript
      ▼
apps/next-app/schema.d.ts
```

エンドポイントを 1 本追加するたびに、ハンドラと `generate-docs` の
2 箇所を人間が更新する必要があった。

なお `main.go` にあった swaggo 形式のコメント (`// @Summary` など) は
swag が依存に入っておらず、どこからも使われていなかったため削除した。

## 検討した選択肢

### A. 現状維持 (Go を正とする) + ドリフト検出

- 利点: 変更が小さい
- 欠点: 手作業が残る。「スキーマ起点」とは呼べない

### B. swaggo を正式導入する

- 利点: 実装とドキュメントが同じ場所にある
- 欠点: コメントの記法に縛られる。コメントの書き忘れは検出できない

### C. yaml を正とし、oapi-codegen でサーバスタブを生成する

- 利点: 要件の文言に忠実。フロントとバックの発生源が完全に一つになる
- 欠点: 移行コストが最も大きい

## 決定

**C を採用した。**

エンドポイントが増えるほど移行コストが上がるため、
Phase 2 (コメント投稿の並行制御) に着手する前に切り替えた。

**変更後:**

```
api/openapi.yaml   ← 人間が編集する唯一の場所
      ├── oapi-codegen ──────→ apps/go-api/internal/httpapi/oapigen/
      └── openapi-typescript ─→ apps/next-app/schema.d.ts
```

### 具体的に何が変わったか

| | 変更前 | 変更後 |
| --- | --- | --- |
| 仕様書の位置 | `apps/go-api/openapi.yaml` (生成物) | `api/openapi.yaml` (手書き・唯一の正) |
| ルーティング定義 | `router.go` に手書き | 生成された `RegisterHandlers` |
| パスパラメータの解析 | `strconv.ParseInt` を手書き | 生成コードが実施 |
| クエリパラメータの解析 | 手書き | 生成コードが実施 |
| レスポンスの型 | ユースケース層の DTO を直接 JSON 化 | 生成された型へ詰め替え |
| 仕様との整合性 | 人間が担保 | **コンパイラが担保** |

### 得られた保証

1. **実装漏れがコンパイルエラーになる**
   `Server` は `oapigen.ServerInterface` を実装する。
   仕様書にエンドポイントを追加して実装を忘れると、ビルドが通らない。

   ```go
   var _ oapigen.ServerInterface = (*Server)(nil)
   ```

2. **レスポンスの形が仕様と一致することをコンパイラが検査する**
   ハンドラは `oapigen.Thread` などの生成型を返す。
   仕様からフィールドを消すと `mapping.go` がコンパイルエラーになる。

3. **仕様書の制約がランタイムで強制される**
   `OapiRequestValidator` ミドルウェアを入れたため、
   `minimum` / `maximum` / `minLength` / `maxLength` / `required` /
   型・enum が実際に検査される。仕様書が飾りではなくなった。

   仕様書に無いパス・メソッドは 404 になる。

   これらは `internal/httpapi/server_test.go` の
   `TestSpecValidation_*` で検証している。

### 残る手作業

**ドメイン層の検証は依然として二重に書く必要がある。**

- 仕様書: `maxLength: 200`
- Go: `model.TitleMaxLength = 200`
- DB: `CHECK (char_length(title) BETWEEN 1 AND 200)`

3 箇所に同じ値が現れる。これは意図的で、それぞれ役割が違う。

- 仕様書 — クライアントへの契約。ミドルウェアが門前払いする
- ドメイン — 空白のみの文字列など、仕様書では表現しにくい規則
- DB — アプリを迂回した書き込みに対する最後の砦

将来ずれる危険はあるため、各所にコメントで対応関係を明記している。

## 結果

- `cmd/generate-docs/` は不要になったため削除した
- `apps/go-api/openapi.yaml` (生成物) も削除した
- CI の `generated-ci` ジョブは oapi-codegen の出力を検査するようになった
- ルーティングを手書きしなくなったぶん、`internal/httpapi` の行数が減った

### 引き受けるコスト

- 仕様書の記法を学ぶ必要がある
- `oapi-codegen` のバージョンを固定して管理する必要がある (`v2.8.0`)
- 生成コードの挙動 (パラメータ束縛の仕様など) を把握しておく必要がある
