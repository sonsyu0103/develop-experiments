# ADR 0005: 認証を Go API 側に置き、匿名投稿と共存させる

- ステータス: **採用 (実装前)**
- 日付: 2026-08-05

[ADR 0003](0003-open-questions.md) の未決 #6 (認証を入れるか) と
#7 (コメント削除 API を公開するか) に決着をつける。

## 背景

現状は認証がなく、誰でもスレッド作成・コメント投稿ができる。
リポジトリ層に `CommentRepository.SoftDelete` を実装済みだが、
「誰が消してよいか」を判定する材料がないため HTTP には公開していない。

マイページ・画像投稿・投稿者表示を入れる以上、認証は避けられない。
**入れる**方向で決定する。

## 決定 1: セッションは Go API 側が持つ

### 検討した選択肢

#### A. Next.js 側で Auth.js (NextAuth) を使う

- 利点: 実装が速い。OIDC のフローをライブラリが全部やる
- 欠点: **Go API が認証について何も知らない状態になる。**
  API は Next から渡ってくるユーザー識別子を信じるしかない。
  つまり API を直接叩けば誰にでもなりすませる。
  Next を「信頼できるプロキシ」として扱うなら API を外部に出せず、
  [ADR 0004](0004-modular-monolith.md) が前提にしている
  「Go API は独立したサービス、Next はそのクライアントの 1 つ」
  という構成と噛み合わない

#### B. Go API 側で OIDC を処理し、サーバ側セッションを持つ (採用)

- 利点: 認証の責任が API に閉じる。Next はブラウザと同じ立場のクライアントになる。
  セッションを失効できる
- 欠点: OIDC のフロー (state / nonce / PKCE / トークン検証) を自前で組む。
  実装量は A より明確に多い

#### C. Go API 側で OIDC を処理し、ステートレスな JWT を発行する

- 利点: セッションストアが不要。水平スケール時に共有状態がない
- 欠点: **失効できない。** 発行済みトークンは有効期限まで生き続ける。
  「ログアウト」「アカウント削除」「権限剥奪」が即座に効かない。
  短命トークン + リフレッシュトークンで緩和できるが、
  リフレッシュトークンは結局サーバ側で失効管理が要るので、
  「ステートレス」という利点が消える

### 決定

**B を採用する。**

Google の OIDC (Authorization Code フロー + PKCE) を Go API 側で処理し、
セッション ID を HttpOnly Cookie で返す。セッションの実体は PostgreSQL に置く。

```
ブラウザ ──① GET /auth/google ──────────▶ Go API
                                            │ state / nonce / code_verifier を生成
        ◀── 302 → accounts.google.com ──────┘

ブラウザ ──② ユーザーが Google で承認 ──▶ Google

ブラウザ ──③ GET /auth/google/callback?code=&state= ──▶ Go API
                                            │ state 照合
                                            │ code → トークン交換 (PKCE)
                                            │ ID トークン検証 (署名/iss/aud/nonce/exp)
                                            │ users に upsert
                                            │ sessions に INSERT
        ◀── 302 → フロント + Set-Cookie ────┘

ブラウザ ──④ GET /me (Cookie 付き) ──────▶ Go API
```

#### セッションストアに Redis ではなく PostgreSQL を使う

Redis の方が定石だが、compose にサービスが 1 つ増え、
永続化方針という別の判断が要る。セッション参照は主キー 1 件の索引アクセスであり、
バッファキャッシュに乗る限りサブミリ秒で返る。

[ADR 0009](0009-scaling-strategy.md) の見積もり (登録 100 万ユーザー) でも、
セッション検索は**ピークで毎秒 2,000 件程度**にとどまる。
PostgreSQL がこれを捌けないということはない。

将来詰まったときに差し替えられるよう、セッションストアは
`comment` モジュールが `ThreadExistenceChecker` を持つのと同じ形で、
利用側が定義するインターフェースの背後に置く。

`sessions` テーブルの定義は [ADR 0016](0016-schema-and-indexes.md) にある
(**この ADR には DDL を書いておらず、集約時に落としかけた**)。

**差し替えを検討する条件**を先に書いておく。

- セッション検索がプライマリの読み取り負荷として無視できない割合を占めたとき
- タスク数が増え、セッション検索が接続数の逼迫に寄与し始めたとき

**セッション検証はリードレプリカに逃がせない。**
レプリケーション遅延のぶんログアウトが効かなくなるため、
読み取りをレプリカに分散しても、この経路だけはプライマリに残る
([ADR 0009](0009-scaling-strategy.md) 決定 2)。
Redis を入れる主な動機は速度ではなく、
**この負荷をプライマリから外せること**になる。

#### Cookie の属性

| 属性 | 値 | 理由 |
| --- | --- | --- |
| `HttpOnly` | あり | JavaScript から読めなくする (XSS でのセッション奪取を防ぐ) |
| `Secure` | 本番のみ | localhost は HTTP なので開発時は付けない |
| `SameSite` | `Lax` | 下記参照 |
| `Path` | `/` | |

`SameSite=Lax` で足りるのは、フロントと API が **same-site** に収まるため。
same-site の判定はスキームと登録可能ドメイン (eTLD+1) で行われ、
**ポートは含まれない**。したがって開発時の `localhost:3000` → `localhost:8080` は
same-site であり、`fetch` でも Cookie が送られる。
理想構成でも `example.com` と `api.example.com` に揃えることで same-site を維持する。

`None` を使わずに済むこの構成を前提とするため、
**API を別ドメインに置く変更はセッション設計の変更を伴う**。
そのときは `SameSite=None; Secure` + CORS の credentials 許可が必要になる。

#### CORS の変更が要る

現在の CORS は許可リスト方式 (既定 `http://localhost:3000`) だが、
Cookie を伴うリクエストには `Access-Control-Allow-Credentials: true` が要る。
このヘッダを付ける場合、`Access-Control-Allow-Origin` に `*` は使えない。
現在すでに許可リスト方式なので、この制約とは元から整合している。

#### Next.js のサーバ側からの呼び出しに注意

サーバコンポーネントや Route Handler から Go API を呼ぶとき、
ブラウザの Cookie は**自動では転送されない**。`cookies()` から読んで
明示的に `Cookie` ヘッダへ載せる必要がある。
ここを忘れると「ブラウザからは動くがサーバ側からは未ログイン扱い」
という切り分けにくい挙動になる。

#### キャッシュに認証済みのレスポンスを載せない

認証が入ると、**同じ URL がユーザーごとに違う内容を返す**ようになる。
これをキャッシュすると、他人の画面が別の人に見える事故になる。

| 層 | 注意点 |
| --- | --- |
| `fetch` のキャッシュ | Next.js 15 以降は既定が `no-store` だが、`force-cache` や `revalidate` を明示すると認証済みレスポンスが共有される |
| ルートのレンダリング | `cookies()` を読んだルートは動的になる。逆に、読まずにユーザー依存の内容を出そうとすると静的化される |
| クライアントの Router Cache | ログアウト後に戻る操作で前の画面が見えることがある。`router.refresh()` で明示的に捨てる |
| CloudFront | 認証済みの応答をキャッシュさせない。API パスはキャッシュ無効にする ([インフラ構成](../infrastructure.md)) |

CDN 層まで含めると 4 か所あり、**どれか 1 つでも漏れると情報漏洩になる**。
「ログイン状態で内容が変わる画面」を追加するたびに、この 4 つを確認する。

## 決定 2: 匿名投稿を残す

ログイン必須にはしない。掲示板としての性格を残す。

### スキーマ

`threads` / `comments` に `author_id BIGINT NULL REFERENCES users(id)` を追加する。

**`NULL` が匿名を意味する。**

`comments` はハッシュパーティション済みだが、パーティション親への
`ALTER TABLE ... ADD COLUMN` は全パーティションに伝播するため、
パーティション構成には手を入れない。

### 権限モデル

| 操作 | 匿名 | ログイン済み | moderator / admin |
| --- | --- | --- | --- |
| スレッド閲覧 / コメント閲覧 | ✅ | ✅ | ✅ |
| スレッド作成 / コメント投稿 | ✅ (`author_id` = NULL) | ✅ (`author_id` = 自分) | ✅ |
| 自分の投稿の削除 | ❌ | ✅ | ✅ |
| 他人・匿名の投稿の削除 | ❌ | ❌ | **✅** |
| 画像の添付 | ❌ ([ADR 0007](0007-image-storage.md)) | ✅ | ✅ |
| 通報 | ❌ | ✅ | ✅ |

これで **[ADR 0003](0003-open-questions.md) の未決 #7 が解ける**。
`SoftDelete` は「`author_id` が現在のセッションのユーザーと一致するとき」
だけ実行でき、HTTP に公開できる。

右端の列は [ADR 0011](0011-moderation.md) で追加した。
**この ADR の初版には管理者が存在せず、
匿名投稿を誰も削除できない設計になっていた。**

### 匿名投稿は後から自分のものにできない

ログイン後に「さっき匿名で書いたやつを自分の投稿にする」機能は**作らない。**

匿名投稿には投稿者を特定する情報がないため、
これを許すと任意の匿名投稿を誰でも自分のものだと主張できてしまう。
Cookie などで暫定的に紐付ける手はあるが、
Cookie は利用者が自由に作れるので防御にならない。

### 削除された投稿と `author_id`

論理削除 (`SoftDelete`) は行を残すため、`author_id` も残る。
将来「アカウント削除」を実装するときは、
`users` の行を消すのではなく匿名化 (`author_id` を NULL にする) する必要がある。
外部キーがあるため、単純な `DELETE FROM users` は失敗する。

## 決定 3: `users` は Google のアカウント ID で識別する

`sub` (Google の一意識別子) を保存し、これで照合する。
**メールアドレスでは照合しない。**

メールアドレスは変更されうるし、Google Workspace ではアカウントを消して
同じアドレスを別人に再割り当てできる。`sub` は再利用されない。

```sql
CREATE TABLE users (
    -- serial ではなく IDENTITY を使う。既存の threads と揃える
    -- (理由は 000001_init_schema.up.sql の冒頭コメント)
    id           BIGINT      GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    -- 外部に見せる識別子。内部 ID を URL に出すとユーザーを列挙できる
    -- (ADR 0003 未決 #11 の決定)。UUID v7 を Go 側で生成する
    public_id    UUID        NOT NULL UNIQUE,
    google_sub   TEXT        NOT NULL UNIQUE,   -- 照合はここ
    email        TEXT        NOT NULL,          -- 表示・連絡用。一意制約は付けない
    display_name TEXT        NOT NULL,
    avatar_url   TEXT,                          -- Google のプロフィール画像 URL (初期値)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**内部 ID (`id`) は API に出さない。** 外部キーとしてのみ使う。
API とフロントが扱うのは `public_id` だけになる。

`author_id` に紐づく投稿者の表示も、`public_id` に解決してから返す。
ここを取り違えると、内部 ID がレスポンスに漏れて列挙が可能になる。

`email_verified` が false の ID トークンは受け付けない。

## モジュール構成

[ADR 0004](0004-modular-monolith.md) に従い、`internal/user` を新しい機能モジュールとして追加する。

```
internal/user/
├── domain/model/        ← User, Session
├── domain/repository/   ← UserRepository, SessionRepository
└── usecase/             ← 認証インタラクター
```

**`.golangci.yml` に depguard ルールを 2 つ (本体用と緩和用) 追加する必要がある。**
ADR 0004 に「忘れると新モジュールだけ無検査になる」と書いてある箇所そのもので、
今回が最初の該当ケースになる。

`thread` / `comment` から `user` を直接 import しない。
投稿者の解決が要る場合は、利用側が必要な操作だけのインターフェースを定義する
(ADR 0004 の `ThreadExistenceChecker` と同じ形)。

## 引き受けるコスト

- **OIDC のフローを自前で持つ。** state / nonce / PKCE / ID トークン検証の
  どれを落としても静かに脆弱になる。ライブラリ (`coreos/go-oidc`) に
  検証を任せ、自前で書く範囲を最小にする
- **セッションの掃除が要る。** 期限切れセッションを消す仕組みがないと
  テーブルが単調増加する
- **テストで外部 IdP に依存させない。** Google を実際に叩くテストは書かない。
  OIDC プロバイダをインターフェースの背後に置き、テストではフェイクを使う
- **`author_id` が NULL 許容であることが、以降のすべてのクエリに効く。**
  投稿者を JOIN する箇所は `LEFT JOIN` になり、表示側は常に
  「匿名」のケースを持つ。ここを取りこぼすと NULL 参照で落ちる
- **秘密情報が初めて入る。** クライアントシークレットとセッション署名鍵。
  compose では環境変数、理想構成では Secrets Manager
  ([インフラ構成](../infrastructure.md))
