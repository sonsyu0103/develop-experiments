# CLAUDE.md

このリポジトリで作業するときの共通指示。

## やりとりの約束

- **日本語で答える。**
- **git の commit / push は、明示的に頼まれるまでやらない。** 差分の確認や
  `git status` などの読み取りは自由。
- **5 分以上かかりそうな処理は、黙って走らせない。** 長いテストやビルドを回すときは
  途中経過を出す。無言で長時間動かれると不安になる。
- **成果物ファイルは `~/claude-artifacts/` に置く。** 一時ディレクトリ (`/private/...`)
  に出されると、あとから探せなくてハマる。リポジトリに入れるべきものは別。
- **引き継ぎや最終検証では「前任が検証できなかった領域」を最初に疑う。**
  「ビルドが通らなかったので未確認」の箇所は、検証網から漏れている。そこが一番危ない。

## プロジェクト構成

| パス | 中身 |
| --- | --- |
| `apps/go-api` | Go の API サーバ。sqlc で SQL からコード生成、oapi-codegen で OpenAPI からスタブ生成 |
| `apps/next-app` | Next.js のフロント。OpenAPI から TypeScript 型を生成 |
| `api/openapi.yaml` | API のスキーマ。**ここが単一の情報源**。ハンドラの型を直接いじらない |
| `apps/go-api/db` | マイグレーションとクエリ |
| `infra` | ログ基盤 (fluent-bit / Athena / DuckDB)、ローカルのエッジ (`caddy`)、AWS の IaC (`terraform`) |
| `docs/adr` | 設計判断の記録。大きめの判断をしたら 1 件足す |

## コマンド

すべて `make`。生の `go test` や `docker compose` を直接叩くより、こちらを使う。

```
make up               # 環境を起動 (マイグレーションも走る)
make check            # CI と同じ検証を一通り。PR を出す前にこれ
make test             # Go のテスト (race detector + カバレッジ)
make lint             # Go / TypeScript の静的解析
make generate         # 生成物をすべて作り直す
make verify-generated # 生成物がコミット済みと一致するか検査
make logs-verify      # ログ基盤が本番と同じ形で動いているか実測 (実環境が要る)
make up-https         # エッジ (TLS 終端) ごと起動する (https://localhost)
make https-verify     # プロキシ配下でしか通らない分岐を実測する
make tf-plan          # AWS に何が作られるかを見る (課金なし)
make tf-apply         # AWS に環境を作る (**課金が発生する**)
make tf-destroy       # AWS の環境を消す
```

**AWS は常時公開しない。** 見せたい日に `tf-apply` して、終わったら
`tf-destroy` する運用にしてある (docs/adr/0024-aws-deployment.md)。
**`tf-apply` と `tf-destroy` は明示的に頼まれるまで実行しない。**

`make help` で全ターゲットが出る。

## 生成コードの扱い

`sqlc` と `oapi-codegen` の出力は**手で編集しない**。スキーマや SQL を直してから
`make generate` を回す。CI の `verify-generated` が差分を検出して落ちる。

## ツールのバージョン

`Makefile` 冒頭で固定している (`SQLC_VERSION` など)。`@latest` にしない
—— ある日ルールが増えて CI が突然赤くなるのを避けるため。更新は意図した変更として
コミットに残す。
