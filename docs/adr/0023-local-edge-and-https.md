# ADR 0023: ローカルにエッジ (TLS 終端) を置く —— 「本番ならこう動く」を実測に変える

- ステータス: **採用 (実装済み)**。`infra/caddy` / `make up-https` /
  `make https-verify` (2026-08-24)
- 日付: 2026-08-24

## 背景

この時点までの `compose.yaml` は**すべて http** だった。
`http://localhost:3000` と `http://localhost:8080` を直接叩く形で、
リバースプロキシは 1 つも居ない。

一方で、実装は本番の姿を先取りして書かれていた。
[インフラ構成](../infrastructure.md) が CloudFront + ALB + ECS を描き、
それを前提にした分岐がコードに入っている:

| 実装 | 場所 | 何を前提にしているか |
| --- | --- | --- |
| `SecureCookie` | `internal/httpapi/auth.go` | 本番は https |
| `X-Forwarded-Proto` によるスキーム判定 | `internal/httpapi/middleware.go` | 前段にプロキシが居る |
| `TRUSTED_PROXIES` | `internal/httpapi/server.go` | ALB が XFF を付け替える |
| `trusted_proxies_unset` の警告 | `cmd/api/main.go` | 本番でプロキシ設定を忘れた場合 |

**この 4 つは、1 度も実行されたことがなかった。**
プロキシが居ないので `X-Forwarded-Proto` は飛んでこないし、
`ENV=development` なので `SecureCookie` は常に `false` 側を通る。
テストは「そう書いてある」ことを確かめられるが、
**設定と設定が噛み合うかは、実際に噛ませないと分からない。**

さらに [ADR 0013](0013-http-defense.md) 決定 2 (セキュリティヘッダ) が
**未実装のまま残っていた**。ADR 自身が
「決定 2 は未実装 (2026-08-19 確認)」と記録している状態で、
`nosniff` も `Referrer-Policy` も応答に付いていない。

## 決定 1: 実 AWS には出さず、ローカルにエッジを置く

`infra/caddy/Caddyfile` に Caddy を置き、`https://localhost` の
単一オリジンで受ける。

```
https://localhost/          -> next-app:3000
https://localhost/api/*     -> go-api:8080   (プレフィックスを剥がす)
https://localhost/images/*  -> minio:9000    (S3 + OAC 相当)
http://localhost/           -> 308 で https へ
```

**本番構成のどれを模しているか:**

| ローカル | 本番 | 何を再現しているか |
| --- | --- | --- |
| Caddy の TLS 終端 | CloudFront / ALB | アプリが平文で受け取り、スキームをヘッダで知る |
| `handle_path /api/*` | CloudFront の Origin Path | プレフィックスを剥がして ALB へ流す |
| Caddy の `header` | CloudFront のレスポンスヘッダポリシー | セキュリティヘッダをエッジで付ける |
| `/images/*` → MinIO | S3 + OAC | 画像を API に通さず返す |

**実 AWS に出さないのは、目的が「公開」ではなく「検証」だから。**
上の 4 つの分岐を通すのに ECS も ALB も要らない。
必要なのは「前段にプロキシが居て、TLS を終端していること」だけになる。

### 既存の経路を塞がない

`edge` には `profiles` を付け、`make up` では起動しない。
`8080` / `3000` の公開もそのまま残す。

**置き換えではなく、本番同型の経路を「もう 1 本」足す。**
エッジ経由でしか再現しない問題を追うときに、
素の経路と見比べられる必要がある —— 差分が出た側が原因になる。

## 決定 2: LocalStack は使わない

「擬似 AWS」の実装として LocalStack が候補になるが、採らない。

1. **肝心の部分が無料枠に無い。** ALB (ELBv2) は Pro 限定、ECS も Base 以上、
   CodePipeline / CodeDeploy も有料枠。
   このリポジトリが検証したいのは**エッジの振る舞い**なので、
   一番欲しいものが手に入らない
2. **擬似実装が二重になる。** S3 は MinIO、SES は Mailpit が既に居る。
   LocalStack を足すと「どちらで検証したのか」が増えるだけで、
   本番との距離は縮まらない

**AWS の API を模すことに価値があるのではなく、
アプリが置かれる状況を模すことに価値がある。**
TLS 終端と XFF の付け替えは、Caddy でも ALB でも同じ形で起きる。

## 決定 3: `SECURE_COOKIE` を `ENV` から独立させる

これまで `SecureCookie` は `ENV` 一本で決まっていた (`!debug`)。
根拠は [ADR 0005](0005-authentication.md) の
「localhost は HTTP なので開発時は付けられない」だった。

**エッジを置いた時点で、この前提が崩れた。**
手元でも `https://localhost` になったので、開発モードのまま Secure を付けられる。
付けられないままだと、**開発環境だけが本番と違う Cookie を配る**ことになり、
Secure 起因の不具合が手元で一度も再現しない。

`LOG_FORMAT` を `ENV` から独立させたのと同じ形にする
(`parseSecureCookie`)。未設定なら従来どおり `ENV` から決めるので、
既存の環境の挙動は変わらない。

**読めない値を `false` に落とさない。** 打ち間違いが
「Secure の付いていない本番」として黙って成立するのを防ぐ。

## 決定 4: セキュリティヘッダはエッジで付ける

[ADR 0013](0013-http-defense.md) 決定 2 は
「`X-Frame-Options` はフロント側の応答に付ける」と書いていたが、
**エッジで付ける**ことにする。

- go-api と next-app の**両方**に漏れなく乗る
- 本番では CloudFront のレスポンスヘッダポリシーが同じ位置に来るので、
  責務の置き場所が本番と揃う
- `next.config` を新設せずに済む (この時点では存在しなかった。その後、本番イメージのために `output: 'standalone'` を置く必要が生じ、[ADR 0024](0024-aws-deployment.md) の 1 で新設した)

CSP は入れない。ADR 0013 が
「まず Report-Only で導入し、違反を観測してから強制へ」と決めており、
観測の受け口 ([ADR 0010](0010-log-pipeline.md) のログ基盤) を
先に用意する必要がある。

## 決定 5: 検査は「壊して効くか」まで見る

`make https-verify` (`.github/scripts/https-probe.sh`) を追加する。

疎通とヘッダの確認だけでは足りない。
**`X-Forwarded-Proto` をわざと落として、書き込みが 403 になることまで確かめる。**
壊しても通るなら、その検査は何も守っていない ——
`LOG_FORMAT=text make logs-verify` で検査の効きを見るのと同じ考え方になる
([ADR 0022](0022-probing-the-checkers.md))。

## 実装して分かったこと (2026-08-24)

### 1. Caddy の環境変数の既定は「未設定なら」であって、空文字には効かない

`Caddyfile` に `{$XFP:https}` と書き、compose 側を `XFP: ${XFP:-}` にしていた。
compose は**空文字を渡す**ので、Caddy から見ると
「環境変数は存在する、値は空」になる。既定値は使われない。

結果、**空の `X-Forwarded-Proto` が go-api に届いた。**
`middleware.go` は `proto != ""` で分岐しているので `else` に落ち、
`c.Request.TLS` は (エッジが終端しているので) `nil`、
スキームは `http` と判定され、`selfOrigin()` が
`http://localhost` を組み立てて **`csrfGuard` が全書き込みを 403** にした。

**既定値は compose 側にも書く。** 二重管理に見えるが、
`${VAR:-}` が生む空文字は「未設定」と同義に扱われないので、
片方だけでは成立しない。

### 2. この壊れ方は、GET を見ている限り気づけない

XFP を落とした状態で測ると、こうなる:

| 経路 | 結果 |
| --- | --- |
| POST (書き込み) | **403** |
| GET (API) | 200 |
| 画面 | 200 |

**ヘルスチェックが GET なら、監視は緑のまま書き込みだけが全滅する。**
`/healthz` は認証も CSRF も通らないので、なおさら気づけない。

### 3. `CORS_ALLOWED_ORIGINS` の明示が、XFP 落ちの保険になっている

`isAllowedOrigin` は**許可リストを先に見る**。
`CORS_ALLOWED_ORIGINS` に自分のオリジンが入っていれば、
`selfOrigin()` まで到達せずに通る。

つまり **2 の壊れ方は「許可リストに自分が入っていない構成」でだけ起きる。**

ADR 0013 の 5 は「同一オリジン構成では運用者が
`CORS_ALLOWED_ORIGINS` を設定する理由がない」と書いていたが、
**理由はある** —— XFP が落ちたときの保険になる。
`make up-https` は単一オリジン構成でも `https://$(SITE_ADDRESS)` を明示する。

この因果があるため、`https-probe.sh` は XFP の効きを
**go-api を直接叩いて**測る。許可リストに無いホスト名を名乗らせて
`selfOrigin()` だけを頼りにさせないと、XFP を落としても通ってしまい、
**検査が「効いている」と誤判定する。**

### 4. HSTS はポートを区別しない

HSTS を `localhost` に付けると、ブラウザは**ホスト単位**で記憶する。
`http://localhost:3000` も `http://localhost:8080` も
以後すべて https へ強制され、**エッジを使わない既存の開発まで止まる。**

既定を `max-age=0` にした。「記憶するな / 記憶済みなら忘れろ」の意味なので、
誤って有効にしたあとの復旧もこの既定で効く。
本番同型の値は `HSTS="max-age=31536000; includeSubDomains" make up-https` で明示する。

ADR 0013 決定 2 の「付けるとブラウザに記憶されて開発が壊れる」は正しかったが、
**ポートを区別しないことまでは書かれていなかった。**

### 5. `AUTH_REDIRECT_URL` のスキーム不一致は、ログインだけを壊す

go-api を手で作り直して `AUTH_REDIRECT_URL` を渡し忘れると、
既定の `http://localhost:8080/auth/google/callback` が使われる。

このとき `/auth/google` は **Secure 付きの `oauth_state` Cookie** を発行し、
Google には **http の redirect_uri** を渡す。
戻ってきたリクエストは http なので Cookie が送られず、state 検証が落ちる。

**API は正常、画面も正常、ログインだけが失敗する。**
`make up-https` は関連する環境変数を一式で渡すが、
**サービスを個別に作り直すと簡単に崩れる。**

### 6. 検査スクリプトに環境を継承させないと、起動待ちを検査結果と取り違える

`https-probe.sh` の中で `docker compose up -d edge` を呼んだとき、
`HTTPS_ENV` を渡していなかったため **go-api が既定値 (http) で作り直され**、
その起動待ちに出た 502 を「検査結果」として読んでしまった。

`make cover` が環境を継承しておらず穴が残っていたのと同じ形になる。
検査スクリプトを呼ぶときは、**検査対象を立てたときと同じ環境で呼ぶ。**

あわせて、待ち条件も直した。`/edge-healthz` はエッジ自身が返すので
**upstream が落ちていても 200 になる。** 待つべきは `/api/healthz` の復帰になる。

### 7. `docker compose down` は profile 付きサービスを止めない

`edge` に `profiles` を付けた結果、**素の `make down` では止まらなくなった。**
エッジだけが 80 / 443 を握ったまま残り、しかも upstream は全部落ちているので、
`https://localhost` は **502 を返し続ける** —— 止めたつもりで止まっていない。

`down` / `clean` に `--profile edge` を付けた。
`down-https` を別に用意する案は採らない。
**「止める」の入り口が 2 つあると、片方だけ実行して同じ状態になる。**

### 8. 検査の前提は、検査自身が保証する

`make down` の直後に `make https-verify` を回すと、
go-api (`go run` のコンパイル) と next-app (初回起動) が上がりきる前に
検査が始まり、**「エッジが振り分けていない」と誤判定した。**

`https-probe.sh` は検査を始める前に
`/edge-healthz` `/api/healthz` `/` の 3 つが揃うまで待つ。
**3 つとも要る** —— それぞれ別のタイミングで上がるため、
どれか 1 つでは足りない。

これは 6 と同じ形の間違いになる。
**検査スクリプトが「呼ばれ方」に前提を置くと、その前提が崩れた日に嘘をつく。**

## 引き受けるコスト

- **compose のサービスが 1 つ増える。** ただし `profiles` 付きなので
  `make up` の起動時間は変わらない
- **証明書がローカル CA になる。** ブラウザは初回に警告を出す
  (`make trust-ca` で消せる)。CA は `caddy-data` ボリュームに永続化するので、
  ボリュームを消すと信頼をやり直すことになる
- **環境変数が 3 つ増えた** (`SECURE_COOKIE` / `TRUSTED_PROXIES` / `XFP`)。
  いずれも既定は従来の挙動と同じで、`make up` には影響しない

## やらないこと

- **実 AWS へのデプロイ。**
  ~~[インフラ構成](../infrastructure.md) の位置づけは変えない~~ ——
  **この判断は同日中に覆した。** ポートフォリオとして
  「構成図が描ける」と「IaC がある」は別物として見られるため、
  [ADR 0024](0024-aws-deployment.md) で Terraform 一式を書き、
  実機で apply → destroy まで回した。
  ここでエッジを立てて実測したことは無駄にならず、
  **ALB が `X-Forwarded-Proto` を上書きする問題の解**がそのまま効いた
  (ADR 0024 の 2)
- **Blue/Green デプロイの模擬。** Caddy の admin API で upstream を
  差し替えれば再現できるが、いまは検証したい分岐が無い。
  `admin` を無効化していないのは、これを後から足せるようにするため
- **CSP の導入。** 決定 4 のとおり、観測の受け口を先に用意する
