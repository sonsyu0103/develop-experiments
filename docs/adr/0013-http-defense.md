# ADR 0013: HTTP 層の防御とエラーコード体系

- ステータス: **採用 (実装前)**
- 日付: 2026-08-05

## 背景

現在のミドルウェアは `gin.Recovery()` / `requestLogger()` / `cors()` の 3 つだけで、
**セキュリティヘッダも CSRF 対策も存在しない**。

認証がなかったため必要がなかっただけであり、
[ADR 0005](0005-authentication.md) で Cookie 認証を入れた瞬間に必要になる。

エラーも `apperr` の 3 種類 (`ErrNotFound` / `ErrInvalidArgument` / `ErrConflict`)
しかなく、認証・権限・レート制限・サイズ超過を表現できない。

## 決定 1: CSRF は `SameSite=Lax` + Origin/Referer 検証の多層防御

### CORS があっても CSRF は防げない

これは取り違えやすいので明記する。

**CORS はブラウザにレスポンスを読ませない仕組みであり、リクエスト自体は飛ぶ。**
サーバ側の防御ではない。

さらに、**プリフライトが起きない要求 (simple request)** が存在する。
`Content-Type` が次のいずれかなら、ブラウザは事前確認なしに本リクエストを送る。

- `application/x-www-form-urlencoded`
- **`multipart/form-data`**
- `text/plain`

[ADR 0007](0007-image-storage.md) で画像アップロードに
`multipart/form-data` を選んだため、**この経路はプリフライトを起こさない**。
JSON の POST なら CORS が効くが、multipart では効かない。

### `SameSite=Lax` だけにも頼らない

`SameSite=Lax` はクロスサイトの POST に Cookie を送らないため、
古典的な CSRF の大部分を防ぐ。ただし穴が残る。

- **サブドメインは same-site 扱い。**
  [インフラ構成](../infrastructure.md) は `example.com` と `api.example.com` を
  使う前提であり、将来別のサブドメインが増えてそこを取られると通る
- `Lax` は**トップレベルナビゲーションの GET** には Cookie を送る

### 決定

3 つを組み合わせる。

| 層 | 内容 |
| --- | --- |
| Cookie | `SameSite=Lax` ([ADR 0005](0005-authentication.md)) |
| **サーバ側検証** | **状態変更メソッド (POST / PUT / PATCH / DELETE) で `Origin` を検証する** |
| 設計原則 | **GET で状態を変更しない** |

検証の手順:

```
1. Origin ヘッダがある → 許可リストと照合。一致しなければ 403
2. Origin がない → Referer のオリジン部分で照合
3. どちらもない → 403 (状態変更メソッドの場合)
```

許可リストは `cors()` が既に持っている (`CORS_ALLOWED_ORIGINS`) ため、
**同じ設定値を共有する**。ただし処理は独立させる ——
CORS はレスポンスヘッダを付ける処理、こちらはリクエストを弾く処理であり、
役割が違う。

3 番目の原則が崩れると `Lax` の前提も崩れるため、
**GET / HEAD に副作用を持たせない**ことを設計上の不変条件とする。
[ADR 0006](0006-view-count-and-popularity.md) で閲覧数の更新を
リクエスト処理から切り離したことは、結果的にこの原則にも寄与している。

### CSRF トークンを使わない理由

同期トークンパターン (フォームごとに秘密値を埋める) はより強固だが、
トークンの発行・保管・SPA での取り回しという実装が増える。

`SameSite=Lax` + Origin 検証 + GET の副作用禁止で、
現代のブラウザに対しては実用的な防御になる。
**この 3 つのうち 1 つでも崩したら、トークン方式を再検討する。**

## 決定 2: セキュリティヘッダを入れる

```
X-Content-Type-Options: nosniff
Referrer-Policy: strict-origin-when-cross-origin
X-Frame-Options: DENY            (フロント側の応答に付ける)
Strict-Transport-Security: ...   (本番のみ)
```

### `nosniff` が特に要る理由

**画像を扱う設計になったため**になる。

これがないと、ブラウザが `Content-Type` を無視して中身から型を推測し、
アップロードされたファイルを別の型として解釈しうる。
[ADR 0007](0007-image-storage.md) の再エンコードで中身は安全になっているが、
多層防御として入れる。

### CSP は段階的に入れる

Content-Security-Policy は最も効果が大きいが、
Next.js のインラインスクリプトと衝突しやすく、
雑に入れると画面が壊れる。

**まず `Report-Only` で導入し、違反を観測してから強制に切り替える。**
違反レポートの送り先は、ログ基盤 ([ADR 0010](0010-log-pipeline.md)) に寄せる。

HSTS は HTTPS でのみ意味を持つため、本番だけに付ける。
ローカルは HTTP なので、付けるとブラウザに記憶されて開発が壊れる。

## 決定 3: エラーコードを gRPC の体系に揃える

現在の `apperr` は 3 つしかない。
既存のコード名 (`INVALID_ARGUMENT` / `NOT_FOUND`) は
gRPC のエラーコードに沿っているため、**そのまま踏襲して拡張する**。

| コード | HTTP | 使う場面 | 出典 |
| --- | --- | --- | --- |
| `INVALID_ARGUMENT` | 400 | 入力値が不正 | 既存 |
| `UNAUTHENTICATED` | **401** | 未ログイン | [ADR 0005](0005-authentication.md) |
| `PERMISSION_DENIED` | **403** | 権限がない / Origin 検証の失敗 | [ADR 0011](0011-moderation.md) |
| `NOT_FOUND` | 404 | 対象が存在しない | 既存 |
| `METHOD_NOT_ALLOWED` | 405 | パスは存在するがメソッドが未定義 | 既存 |
| `CONFLICT` | 409 | 直列化失敗・一意制約違反・冪等キーの処理中 | 既存 |
| `PAYLOAD_TOO_LARGE` | **413** | 画像サイズ超過 | [ADR 0007](0007-image-storage.md) |
| `FAILED_PRECONDITION` | **422** | 同じ冪等キーで別の内容が送られた | [ADR 0015](0015-idempotency.md) |
| `RESOURCE_EXHAUSTED` | **429** | レート制限 | [ADR 0008](0008-contact-and-mail.md) |
| `INTERNAL` | 500 | 想定外 | 既存 |

### 401 と 403 を取り違えない

実装で最も間違えやすい箇所になる。

- **401 `UNAUTHENTICATED`** —— **誰か分からない**。ログインすれば解決する
- **403 `PERMISSION_DENIED`** —— **誰かは分かるが、権限がない**。ログインしても解決しない

フロントはこれで挙動を変える。401 ならログイン画面へ、403 ならエラー表示になる。
**取り違えると、権限のないユーザーがログイン画面に飛ばされ続ける**。

### 存在を隠すべき場合は 404 を返す

他人の下書きや非公開リソースに対して 403 を返すと、
**「存在すること」自体が漏れる**。
権限がないことを隠したい対象については 404 を返す。

現時点の設計では公開掲示板なので該当しないが、
将来非公開の概念を入れたときに必要になる。

### 追加は `api/openapi.yaml` から

`oapigen.ErrorErrorCode` は仕様書から生成される enum であり、
[ADR 0002](0002-openapi-direction.md) に従って
**仕様書に enum を追加してから `make generate`** する。
`apperr` の番兵エラーと 1 対 1 で対応させる。

## ミドルウェアの順序

順序を誤ると防御が効かない。

```go
r.Use(
    gin.Recovery(),        // 1. パニックを最後まで拾う
    requestLogger(),       // 2. 弾かれた要求もログに残す
    securityHeaders(),     // 3. すべての応答に付ける (エラー応答にも)
    cors(allowedOrigins),  // 4. プリフライトはここで完結する
    csrfGuard(allowedOrigins), // 5. CORS の後 (OPTIONS を通してから)
    // 認証はこの後
)
```

**`requestLogger` を前に置く**のは、CSRF で弾いた要求も記録するため。
攻撃の観測にはこれが要る。

**`securityHeaders` を CORS より前に置く**のは、
エラー応答やプリフライト応答にもヘッダを付けるため。

## 実装して分かったこと (2026-08-14)

決定 1 と決定 3 を実装した。決定 2 (セキュリティヘッダ) はまだ入っていない。

### 1. **決定 1 だけ読んで実装すると、エラーコードを間違える**

`csrfGuard` を書いたとき、403 のコードとして `FORBIDDEN` を新設した。
**決定 3 の表が既に `PERMISSION_DENIED` と決めており、しかも用途の欄に
「権限がない / Origin 検証の失敗」と名指しで書いてあった。**

原因は、決定 1 の節だけを見て実装に入ったこと。決定 3 は
「gRPC の体系に揃える」が趣旨なので、gRPC に存在しない `FORBIDDEN` は
体系から外れる。仕様書・生成物・スモークを直した。

**1 つの ADR の中でも、決定どうしが噛み合っている。**
実装に入る前に、その ADR の表を全部見ること。

なお `components/responses` の名前は `Forbidden` のままにした ——
こちらは HTTP のステータス名であり、`BadRequest` / `NotFound` と同じ流儀になる。

### 2. ミドルウェアの並びは上の擬似コードと違う

実際の並びは
`requestID` → `requestLogger` → `recovery` → `cors` → `csrfGuard` → `bodyLimit`。

- `securityHeaders` はまだ無い (決定 2 が未実装)
- `recovery` は `requestLogger` の**内側**。外側に置くとパニックが
  `requestLogger` の `c.Next()` を巻き戻して抜け、`http_request` の行が
  1 本も出なかった (実測)
- `csrfGuard` は `bodyLimit` の**前**。弾くと決まっている要求の本文を
  読み始める理由が無い。[ADR 0007](0007-image-storage.md) の画像は
  最大 5 MiB を受けるので、逆だと拒否のために読み切ることになる

### 3. 「Origin が無ければ通す」にはできない

手順 3 (どちらも無ければ 403) を実装すると、**API を叩くものすべてに影響する。**
スモークテストと `curl` も含まれる。

一瞬「ブラウザ以外は素通しでよいのでは」と考えたが、**それでは防御にならない。**
Origin を送らないだけで検証を迂回できる。スモーク側に Origin を付ける
(`SMOKE_ORIGIN`、既定は `CORS_ALLOWED_ORIGINS` の既定値と同じ) 形にした。

### 4. Referer の照合を前方一致で書いてはいけない

`https://example.com.evil.test/` は `https://example.com` で始まる。
`url.Parse` して `scheme://host` を組み直してから完全一致で照合する。

### 5. **設定漏れが「全書き込み停止」に変わる**

`csrfGuard` は `CORS_ALLOWED_ORIGINS` を共有する。既定値は開発用の
`http://localhost:3000` なので、**設定せずに本番へ出すと書き込みが全滅する。**

この決定より前は、同じ設定漏れが「レスポンスヘッダが付かないだけ」で済んでいた。
しかもフロントと API を 1 つのホストに置く構成では CORS が本当に不要なので、
**運用者にはこの変数を設定する理由が無い。**

`csrfGuard` は**自分自身のオリジンを常に許す**ようにした。
`Origin` が要求先の `scheme://host` と一致すれば通す。

**`Host` を信用してよいのか** —— この検査が守る相手はブラウザだけになる。
ブラウザが送る `Host` は「利用者が開いた URL」であり、攻撃者は変えられない
(`evil.test` のページから `example.com` へ投げても `Host` は `example.com`、
`Origin` は `evil.test`)。ブラウザ以外は `Origin` を自由に詐称できるので、
そもそもこの検査の射程外になる。

scheme は `X-Forwarded-Proto` を見て、無ければ TLS の有無で決める。
**逆プロキシの内側でしか正しくない** —— 直接晒す構成では手前で剥がすこと
(`X-Request-Id` を尊重しているのと同じ前提)。

### 6. `cors()` の許可メソッド・許可ヘッダが仕様書と揃っていなかった

`Access-Control-Allow-Methods` は `GET, POST, OPTIONS` のままで、
**`PUT` が入っていなかった。** `PUT /me/avatar` は preflight でブラウザに
弾かれるので、**フロントから一度も呼べない。** 仕様書に 403 を宣言したのに
到達できない状態になっていた。

`Idempotency-Key` も `Access-Control-Allow-Headers` に無く、
[ADR 0015](0015-idempotency.md) の冪等キーをブラウザから送れなかった。

決定 1 が「書き込みはブラウザから直接叩く」を既定にしたことで、
**それまで潜んでいた穴が確実な故障に変わった。**
仕様書にメソッドやヘッダを足したら `cors()` も見ること。

### 7. **Server Components からの書き込みは 403 になる**

現在フロントが叩いているのは `GET /threads` だけなので影響が無いが、
**Server Actions や Route Handler から書き込みを始めた瞬間に落ちる。**
サーバ側の `fetch` は `Origin` を送らないためで、ブラウザからの
リクエストとは扱いが変わる。

書き込みは**ブラウザから直接叩く**か、サーバ側から叩くなら `Origin` を
明示的に付ける。前者を既定にする —— サーバ側から付けられる `Origin` は
自己申告であり、検証としての意味が薄い。

## 引き受けるコスト

- **Origin ヘッダに依存する。** 古いブラウザや一部のプロキシは送らないことがある。
  その場合 Referer にフォールバックし、両方なければ拒否するため、
  **特殊な環境からの書き込みが弾かれうる**
- **CSP の導入が段階的になる。** `Report-Only` の期間は防御になっていない。
  いつ強制に切り替えるかを決めておかないと、そのまま放置される
- **エラーコードが増えると、フロントの分岐も増える。**
  仕様書の enum が増えるたびに、TypeScript 側の網羅性を確認する必要がある
- **ミドルウェアの順序が暗黙の前提になる。**
  並べ替えると静かに防御が外れるため、順序の理由をコードのコメントに残す
