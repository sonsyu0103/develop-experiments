// Playwright の設定。
//
// 【何を測るために入れたか】
// 管理画面のレビューで出た指摘 6 件のうち **4 件が、
// 「応答が遅れた / 失敗したときのクライアントの状態」**だった。
//
//   - 古い応答が新しい一覧に追記される (絞り込みを変えた直後)
//   - 削除の二重送信が通る (行をまたいだとき)
//   - 一過性の取得失敗が「読み込み中」で固定される
//   - 取得失敗時に「通報はありません」がエラーと同時に出る
//
// `tsc` も `eslint` も `next build` も、**どれ 1 つ捕まえられない。**
// 型が通ることは「呼べる」ことしか言わない。
//
// 【なぜ実バックエンドを使わないか】
// **上の 4 つは実バックエンドでは再現できない。**
// 「A の応答が B より後に届く」を作れないためになる。
// ここで測りたいのは API の挙動ではなく、
// **遅延・失敗・順序に対するクライアントの状態遷移**なので、
// 応答は `page.route` で差し替える。
//
// API 側を実 HTTP 越しに見るのは `make smoke` の役目で、そちらは
// 実 DB と実 MinIO に対して 207 件を回している。**重ねない。**
// 結果として Go API も Postgres も MinIO も要らず、
// CI の Next.js ジョブにそのまま乗る。
//
// 【NEXT_PUBLIC_API_URL を空にしている理由】
// 空文字にすると API の宛先が相対 URL (`/me`) になり、**同一オリジン**になる。
// 別オリジンのままだと `page.route` で差し替えた応答にも
// ブラウザが CORS を要求し、preflight と許可ヘッダの再現が要る ——
// **この設定ファイルで CORS を偽装することになる。**
// そこは cors() の単体検査とスモークの preflight 検査が持っている領域なので、
// ここでは踏まない。
//
// なお同一オリジン構成は実在する形になる (フロントと API を 1 つのホストに
// 置く場合。docs/adr/0013-http-defense.md の「実装して分かったこと 5」)。
import { defineConfig, devices } from '@playwright/test';

const port = 3100;

export default defineConfig({
  testDir: './e2e',

  // **本番ビルドに対して回す。** `next dev` は Strict Mode で
  // 効果が 2 回走るなど挙動が違い、`/admin` が静的化されることも確かめられない。
  webServer: {
    command: `npm run build && npx next start --port ${port}`,
    url: `http://127.0.0.1:${port}/admin`,
    reuseExistingServer: false,
    timeout: 120_000,
    env: {
      // 上記のとおり、相対 URL にして同一オリジンにする。
      NEXT_PUBLIC_API_URL: '',
      // Server Component の /threads 取得先。**繋がらなくてよい** ——
      // 管理画面のテストはトップページを開かない。
      API_URL: 'http://127.0.0.1:9',
    },
  },

  use: {
    baseURL: `http://127.0.0.1:${port}`,
    // 落ちたときに何が見えていたかを残す。
    trace: 'retain-on-failure',
  },

  // **CI では再試行しない。** ここで測るのは状態遷移であり、
  // 再試行で緑になるなら、それは「たまたま順序が変わった」ことを意味する。
  // 隠すと、いま塞いだばかりの競合がそのまま戻る。
  retries: 0,
  // 順序に依存する検査を書いていないので並列でよい。
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? 'list' : [['list'], ['html', { open: 'never' }]],

  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
