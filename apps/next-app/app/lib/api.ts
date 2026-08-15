// Go API を叩くクライアント。
//
// 【ブラウザからだけ呼べる】
// **Server Components / Server Actions からは呼べません** ——
// サーバ側の `fetch` は `Origin` を送らないため、状態変更メソッドは
// `csrfGuard` に 403 で弾かれます
// (docs/adr/0013-http-defense.md の「実装して分かったこと 7」)。
//
// ADR 0013 はそこで「書き込みはブラウザから直接叩く」を既定に決めています。
// サーバ側から `Origin` を自分で付ける形も取れますが、
// **自己申告の `Origin` は検証として意味を持たない**ためです。
//
// 読み取りも同じ経路に揃えました。理由は 2 つあります。
//
//   - サーバ側から読むと Cookie が自動転送されず、`cookies()` から
//     読んで明示的に載せる必要がある (ADR 0005)。
//     **読みと書きで経路が分かれるほうが間違えやすい**
//   - **キャッシュの層が 1 つに減る** (下記)
//
// 【4 層キャッシュがここでは 1 層になる】
// ADR 0005 は「認証済みの応答をキャッシュに載せない」ために
// 4 か所 (fetch / ルートのレンダリング / Router Cache / CDN) を
// 確認せよと決めています。**管理画面で最も厳しく効く**箇所です。
//
// 取得をすべてブラウザに寄せると、サーバが返す HTML と RSC ペイロードに
// **利用者ごとの内容が 1 つも入りません。** 残る層はこの `fetch` だけになり、
// ここで `cache: 'no-store'` を効かせれば済みます。
// ルートを動的にする必要も、CDN で管理画面のパスを避ける必要もありません
// —— 静的なまま配っても、中身が空だからです。
import type { components } from '../../schema';

type Schemas = components['schemas'];

export type Me = Schemas['Me'];
export type Role = Schemas['Role'];
export type Report = Schemas['Report'];
export type ReportList = Schemas['ReportList'];
export type ReportStatus = Schemas['ReportStatus'];
export type ReportReason = Schemas['ReportReason'];
export type ReportTargetType = Schemas['ReportTargetType'];
export type Thread = Schemas['Thread'];
export type ModerationAction = Schemas['ModerationAction'];
export type ModerationDeleteAction = Schemas['ModerationDeleteAction'];

/** エラーの機械可読な種別。**文言ではなくこの値で分岐します。** */
export type ErrorCode = Schemas['Error']['error']['code'];

// **ブラウザから叩くので NEXT_PUBLIC_ が要ります。**
// サーバ間通信用の API_URL (コンテナ名で解決する) はブラウザから引けません。
//
// 【**ビルド時に焼き込まれます。** 実行時に読めません】
// `NEXT_PUBLIC_*` は `next build` の時点で定数へ置換されるので、
// **`page.tsx` とは挙動が違います** —— あちらは Server Component なので
// `process.env` を実行時に読み、コンテナの環境変数がそのまま効きます。
//
// つまり `NEXT_PUBLIC_API_URL` を渡さずに本番ビルドすると、
// この既定値がバンドルに焼き付き、**管理画面だけが利用者自身の PC を
// 叩きます** (`http://localhost:8080`)。しかもビルドは成功し、
// 壊れていることが分かるのはブラウザで開いたときになる。
//
// 現状は Dockerfile が `npm run dev` しか使わないので踏んでいません。
// **本番のイメージを作る段になったら、ビルド引数として渡すこと。**
const apiUrl = process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080';

/**
 * API が返したエラー。
 *
 * **`code` で分岐してください。** `message` は人間向けで、
 * 予告なく変わります (仕様書の `Error` スキーマ)。
 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: ErrorCode,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/**
 * 応答から `ApiError` を組み立てます。
 *
 * **本文が仕様どおりとは限りません。** 逆プロキシや Next.js 自身が
 * 割り込んだ応答は JSON ですらないことがあるので、
 * 読めなければステータスから埋めます —— ここで例外を投げると、
 * 本当のエラーが「JSON の解析に失敗」にすり替わります。
 */
async function toApiError(res: Response): Promise<ApiError> {
  const fallback: ErrorCode = res.status === 401 ? 'UNAUTHENTICATED' : 'INTERNAL';
  try {
    const body = (await res.json()) as Schemas['Error'];
    if (body?.error?.code) {
      return new ApiError(res.status, body.error.code, body.error.message);
    }
  } catch {
    // 下の fallback へ落とす
  }
  return new ApiError(res.status, fallback, `${res.status} ${res.statusText}`);
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // **サーバ側から呼ばれたら、その場で落とします。**
  // 黙って通すと、書き込みは 403、読み取りは「なぜか未ログイン」になり、
  // どちらも原因から遠い場所で症状が出ます (ADR 0005 / 0013)。
  if (typeof window === 'undefined') {
    throw new Error(
      `api.ts はブラウザからだけ呼べます (${path})。` +
        'Server Components から叩くと Origin が付かず 403 になります ' +
        '(docs/adr/0013-http-defense.md の 7)。',
    );
  }

  const res = await fetch(`${apiUrl}${path}`, {
    ...init,
    // **セッション Cookie を送るために要ります。**
    // 既定 (same-origin) では、API が別オリジンにいる構成で送られません。
    credentials: 'include',
    // 認証済みの応答をキャッシュに載せない (ADR 0005)。
    // Next.js 15 以降は既定が no-store だが、既定に頼らず明示する ——
    // 既定が変わったときに静かに壊れる側の設定なので。
    cache: 'no-store',
  });

  if (!res.ok) {
    throw await toApiError(res);
  }
  // 204 を返す経路のために本文の有無を見る。
  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

function jsonBody(method: string, body: unknown): RequestInit {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  };
}

/**
 * ログインを開始する URL。
 *
 * **`fetch` ではなくページ遷移で使います。** Google へ 302 で送る経路なので、
 * `fetch` で辿ると Cookie (state / nonce / PKCE) がブラウザに残りません。
 */
export function loginUrl(): string {
  return `${apiUrl}/auth/google`;
}

/**
 * ログイン中の利用者を返します。**未ログインでは 401** です。
 *
 * `role` は自分のぶんだけ返ります —— 管理用の導線を出し分けるためです。
 */
export function getMe(): Promise<Me> {
  return request<Me>('/me');
}

/**
 * スレッドを 1 件取得します。**論理削除済みは 404** です。
 *
 * 通報キューで「何が通報されたか」を出すために使います ——
 * 通報が持っているのは ID だけなので、これが無いと
 * モデレーターは数字だけを見て判断することになります。
 */
export function getThread(id: number): Promise<Thread> {
  return request<Thread>(`/threads/${id}`);
}

/**
 * 通報キューを取得します。**古い順** (`id` 昇順)。
 *
 * **他の一覧と向きが逆です。** 滞留したときに最初に届いたものから
 * 処理されるようにするためで、取り違えると「次ページが常に空」になります
 * (ADR 0011 の「実装して分かったこと 6」)。
 */
export function listReports(params: {
  status?: ReportStatus;
  cursor?: string;
  size?: number;
}): Promise<ReportList> {
  const q = new URLSearchParams();
  if (params.status) q.set('status', params.status);
  // **カーソルは不透明トークンです** (ADR 0018)。
  // 中身を解釈も生成もせず、返ってきた値をそのまま返します。
  if (params.cursor) q.set('cursor', params.cursor);
  if (params.size) q.set('size', String(params.size));

  return request<ReportList>(`/moderation/reports?${q.toString()}`);
}

/**
 * 通報を処理済みにします。
 *
 * **投稿には触れません。** 削除は `createModerationAction` を別に呼びます ——
 * 「通報を却下する」と「投稿を消す」は別の判断で、
 * まとめるとキューを片付ける操作がそのまま削除になります (仕様書)。
 */
export function resolveReport(
  id: number,
  status: 'resolved' | 'rejected',
): Promise<Report> {
  return request<Report>(`/moderation/reports/${id}`, jsonBody('PATCH', { status }));
}

/**
 * 投稿・画像を削除し、監査記録を残します。
 *
 * **`delete_comment` には `threadId` が要ります。** `comments` は
 * `thread_id` による HASH パーティションで主キーが `(thread_id, id)` なので、
 * コメント ID だけでは 8 パーティションすべてを走査します (ADR 0011)。
 */
export function createModerationAction(body: {
  action: ModerationDeleteAction;
  targetId: string;
  threadId?: number;
  reason?: string;
}): Promise<ModerationAction> {
  return request<ModerationAction>('/moderation/actions', jsonBody('POST', body));
}

/**
 * 利用者のロールを変更します。**admin だけ**が呼べます。
 *
 * 受け付けるのは**公開 ID** です (ADR 0003 未決 #11)。
 * 返ってくる `targetId` も公開 ID なので、そのまま他の API へ渡せます ——
 * `moderation_actions` に記録される値 (内部 ID) とは別物です。
 */
export function changeUserRole(
  publicId: string,
  body: { role: Role; reason?: string },
): Promise<ModerationAction> {
  return request<ModerationAction>(
    `/users/${encodeURIComponent(publicId)}/role`,
    jsonBody('PATCH', body),
  );
}
