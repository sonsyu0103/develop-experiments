# go-service

`make new-service` で作った Go の HTTP サービス。

## 最初の 5 分

```sh
make test         # テスト
make run          # http://localhost:8080/hello?name=you
make image        # 本番用イメージ (distroless)
make kind-deploy  # kind のクラスタに載せる (CLUSTER=kind)
```

## 最初から入っているもの

| もの | 中身 |
| --- | --- |
| `api/openapi.yaml` | **API の唯一の正。** ここを直して `make generate`。ハンドラは生成された `ServerInterface` を実装するので、実装し忘れるとコンパイルが通らない |
| リクエスト検証 | 仕様書の `maxLength` などが実際に強制される。仕様に無いパスは 404、メソッドは 405 |
| `/healthz` と `/readyz` | 生存と受付可否を分けてある。**依存先 (DB など) を足したら `/readyz` だけで見る** |
| 停止の作法 | k8s では `preStop` で 5 秒待って振り分け先から外れてから SIGTERM。処理中のリクエストを最大 20 秒待って止まる |
| `Dockerfile` | dev / builder / prod の 3 段。prod は distroless・非 root (UID 65532) |
| `deploy/k8s` | 非 root・読み取り専用のルート・権限の全削除・`/tmp` だけ書ける。requests あり、CPU の limits なし |
| `.github/workflows/ci.yml` | 生成物のずれ・lint・テスト・govulncheck・イメージのビルド・マニフェストの組み立て |

## 変えてよいもの・変えるときに気をつけるもの

- **既定値は「何も考えずに使っても安全側」に倒してある。** 外すときは、理由をそのファイルのコメントに残すこと
- Go の版は `go.mod` の `toolchain`、`Dockerfile` の `FROM`、`ci.yml` の `GO_VERSION` の **3 か所を揃えて**上げる
- ツールの版は固定してある (`oapi-codegen` は `go.mod` の `tool`、golangci-lint と govulncheck は `Makefile` の冒頭)。`ci.yml` には書かない。`@latest` にしない
- 生成物 (`internal/httpapi/oapigen`) は手で編集しない

## ひな形との関係

このサービスは生成した時点のひな形の写しで、**ひな形が更新されても自動では追従しない。**
更新を取り込みたいときは、ひな形の変更履歴を見て手で当てる。
