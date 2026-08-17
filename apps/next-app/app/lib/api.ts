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
export type ThreadList = Schemas['ThreadList'];
export type Comment = Schemas['Comment'];
export type CommentList = Schemas['CommentList'];
export type CreateThreadRequest = Schemas['CreateThreadRequest'];
export type CreateCommentRequest = Schemas['CreateCommentRequest'];
export type CreateReportRequest = Schemas['CreateReportRequest'];
export type Author = Schemas['Author'];
export type Image = Schemas['Image'];
export type ImageKind = Schemas['ImageKind'];
export type ModerationAction = Schemas['ModerationAction'];
export type ModerationDeleteAction = Schemas['ModerationDeleteAction'];
export type CreateContactRequest = Schemas['CreateContactRequest'];
export type ContactAccepted = Schemas['ContactAccepted'];

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
 * ログアウトします。**セッションは即座に無効になります**
 * (実体を DB に持つため。ADR 0005 決定 1)。
 *
 * 呼んだあとは `invalidateMe()` を必ず通してください ——
 * こちらは Cookie を捨てるだけで、画面が握っている `Me` は残ります。
 */
export function logout(): Promise<void> {
  return request<void>('/auth/logout', { method: 'POST' });
}

/**
 * プロフィール画像を設定します。**`null` で解除**します
 * (Google のプロフィール画像に戻ります)。
 *
 * **他人の画像 ID は 404** です —— 403 だと「その ID の画像が
 * 存在すること」自体が漏れるためです (ADR 0013)。
 */
export function setMyAvatar(imageId: string | null): Promise<Me> {
  return request<Me>('/me/avatar', jsonBody('PUT', { imageId }));
}

/**
 * 画像をアップロードします。**添付は別の操作です** ——
 * ここで得た `id` を投稿側に渡してください。
 *
 * 【`Content-Type` を自分で付けません】
 * `multipart/form-data` は境界文字列 (boundary) をヘッダに含む必要があり、
 * それを知っているのは `FormData` を組み立てたブラウザだけです。
 * 手で `multipart/form-data` と書くと boundary が落ち、
 * **サーバは本文を 1 つも読めません。** `jsonBody` と分けてあるのはこのためです。
 *
 * 【`Idempotency-Key` は送りません】
 * 仕様がこの経路だけ受け付けません (ADR 0015 決定 3 の要求を
 * DB とストレージにまたがる操作では満たせないため)。二重アップロードで
 * 起きるのは「使われない画像が 1 枚増える」ことだけです。
 */
export function uploadImage(file: File, kind: ImageKind): Promise<Image> {
  const form = new FormData();
  form.append('file', file);
  form.append('kind', kind);
  return request<Image>('/images', { method: 'POST', body: form });
}

/**
 * スレッドを立てます。**未ログインでも立てられます** (匿名投稿)。
 *
 * ただし `iconImageId` を付ける場合はログインが要ります
 * (画像の投稿にログインが要るため。401)。
 */
export function createThread(body: CreateThreadRequest): Promise<Thread> {
  return request<Thread>('/threads', jsonBody('POST', body));
}

/**
 * 自分のスレッドを削除します。論理削除です。
 *
 * **匿名で立てたスレッドは消せません** (403)。`author_id` が NULL で、
 * 本人であることを示せないためです (ADR 0005 決定 2)。
 */
export function deleteThread(id: number): Promise<void> {
  return request<void>(`/threads/${id}`, { method: 'DELETE' });
}

/**
 * コメント一覧を取得します。**新しい順**です。
 *
 * `cursor` は不透明トークンなので、解釈も生成もせず往復させます (ADR 0018)。
 */
export function listComments(
  threadId: number,
  params: { cursor?: string; size?: number } = {},
): Promise<CommentList> {
  const q = new URLSearchParams();
  if (params.cursor) q.set('cursor', params.cursor);
  if (params.size) q.set('size', String(params.size));
  const search = q.size === 0 ? '' : `?${q.toString()}`;

  return request<CommentList>(`/threads/${threadId}/comments${search}`);
}

/**
 * コメントを投稿します。
 *
 * 【`idempotencyKey` は再送のあいだ変えないでください】
 * 二重投稿を防ぐのはボタンの無効化ではなく**このキー**です
 * (ADR 0015 決定 1)。「タイムアウトしたが実は成功していた」場合、
 * 画面には失敗と出ますが、サーバには 1 件入っています。
 * 同じキーで送り直せば前回の結果がそのまま返り、2 件目は作られません。
 *
 * **内容を変えたら新しいキーにしてください。** 同じキーで別の本文を送ると
 * 422 になります —— 黙って前回の結果を返すと、クライアントのバグが
 * 見えなくなるためです。
 *
 * **未ログインでは無視されます** (同 決定 4)。匿名にはキーの名前空間を
 * 分ける手段が無く、IP で分けると NAT の背後で他人と衝突します。
 */
export function createComment(
  threadId: number,
  body: CreateCommentRequest,
  idempotencyKey?: string,
): Promise<Comment> {
  // **`jsonBody` を展開しません。** `RequestInit['headers']` は
  // `Headers` や配列も取りうる型なので、展開すると型が合いません。
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (idempotencyKey !== undefined && idempotencyKey !== '') {
    headers['Idempotency-Key'] = idempotencyKey;
  }

  return request<Comment>(`/threads/${threadId}/comments`, {
    method: 'POST',
    headers,
    body: JSON.stringify(body),
  });
}

/**
 * 自分のコメントを削除します。
 *
 * **`threadId` が要ります。** `comments` は `thread_id` による
 * HASH パーティションで主キーが `(thread_id, id)` なので、
 * コメント ID だけでは 8 パーティションすべてを走査します。
 *
 * 削除しても `seq` (レス番号) は再利用されません ——
 * 再利用すると過去の `>>5` が別の投稿を指すようになります
 * (ADR 0019 決定 5)。
 */
export function deleteComment(threadId: number, commentId: number): Promise<void> {
  return request<void>(`/threads/${threadId}/comments/${commentId}`, { method: 'DELETE' });
}

/**
 * 投稿を通報します。**ログインが必要です** (ADR 0011 決定 4)。
 *
 * 投稿は匿名を許すのに通報は許さないのは、投稿が表現であるのに対し
 * 通報は他人への申し立てだからです。匿名で受け付けると、
 * 通報そのものがキューを埋める荒らしの手段になります。
 *
 * **同じ対象への 2 回目はエラーになりません** —— 最初の通報が
 * 200 でそのまま返ります。返り値の `status` が `open` でなければ、
 * その通報は既に処理済みです (キューには積まれていません)。
 */
export function createReport(body: CreateReportRequest): Promise<Report> {
  return request<Report>('/reports', jsonBody('POST', body));
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
 * 問い合わせを送ります。**ログインは不要です。**
 *
 * 返る `202` は**受理であって送信完了ではありません**
 * (docs/adr/0008-contact-and-mail.md 決定 1)。メールは定期処理が
 * 後から送るので、画面の文言も「受け付けました」に留めてください ——
 * 「送信しました」と書くと、送信が失敗しても利用者は成功したと思ったままになります。
 *
 * **`website` は honeypot です。** 呼び出し側は空文字を送ってください。
 * 値が入っていると API 側で破棄されますが、応答は成功と同じです
 * (エラーにするとボットに引き金を教えることになるため)。
 */
export function submitContact(body: CreateContactRequest): Promise<ContactAccepted> {
  return request<ContactAccepted>('/contact', jsonBody('POST', body));
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
