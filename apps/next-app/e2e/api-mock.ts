// 管理画面が叩く API を差し替えるための道具。
//
// **応答の形は schema.d.ts の型で縛る。** 手書きの JSON を置くと、
// 仕様書が変わったときにテストだけが古い形のまま緑になる ——
// 「テストが通っているのに画面が壊れている」という、
// いちばん困る状態を作れてしまう。
import type { Page, Route } from '@playwright/test';

import type { components } from '../schema';

type Schemas = components['schemas'];

export type Me = Schemas['Me'];
export type Report = Schemas['Report'];
export type ReportList = Schemas['ReportList'];
export type Thread = Schemas['Thread'];
export type Comment = Schemas['Comment'];
export type CommentList = Schemas['CommentList'];
export type ErrorCode = Schemas['Error']['error']['code'];

export const moderator: Me = {
  publicId: '01920000-0000-7000-8000-0000000000aa',
  displayName: 'モデレーター',
  // 実在しないドメインにする (検査用の値が本物に見えないように)。
  email: 'moderator@example.invalid',
  role: 'moderator',
};

export const admin: Me = { ...moderator, role: 'admin', displayName: '管理者' };
export const plainUser: Me = { ...moderator, role: 'user', displayName: '一般利用者' };

/** 通報 1 件。既定はスレッドへの未処理の通報。 */
export function report(over: Partial<Report> = {}): Report {
  return {
    id: 1,
    targetType: 'thread',
    targetId: 100,
    reason: 'spam',
    note: null,
    status: 'open',
    createdAt: '2026-08-16T00:00:00Z',
    resolvedAt: null,
    ...over,
  };
}

export function thread(over: Partial<Thread> = {}): Thread {
  return {
    id: 100,
    title: 'テスト用のスレッド',
    commentCount: 0,
    // 閲覧数は概算値 (docs/adr/0006-view-count-and-popularity.md)。
    // 管理画面の検査では中身を見ないが、**必須フィールドなので省けない** ——
    // 省くと「API が返さない形」をモックしたことになる。
    viewCount: 0,
    // **匿名投稿は author が null。** 退会済みとは別物になる
    // (退会済みは publicId がフィールドごと省略される)。
    author: null,
    icon: null,
    createdAt: '2026-08-16T00:00:00Z',
    ...over,
  };
}

export function list(reports: Report[], nextCursor: string | null = null): ReportList {
  return { reports, nextCursor };
}

/**
 * コメント 1 件。既定は**匿名**の投稿。
 *
 * **`author` と `image` は必須**なので省けない ——
 * 省くと「API が返さない形」をモックしたことになる。
 */
export function comment(over: Partial<Comment> = {}): Comment {
  return {
    id: 1,
    threadId: 100,
    seq: 1,
    // **匿名の表示名。** `author` があるときは、そちらを優先して表示する
    // (ログイン中の投稿では、この欄は既定値のまま保存される。ADR 0014)。
    authorName: '名無しさん',
    author: null,
    body: 'テスト用のコメント',
    createdAt: '2026-08-16T00:00:00Z',
    image: null,
    ...over,
  };
}

/** 指定の利用者が書いたコメントにする。 */
export function byUser(me: Me, over: Partial<Comment> = {}): Comment {
  return comment({
    author: { publicId: me.publicId, displayName: me.displayName, withdrawn: false },
    ...over,
  });
}

export function comments(items: Comment[], nextCursor: string | null = null): CommentList {
  return { comments: items, nextCursor };
}

/**
 * `GET /threads/{id}/comments` を差し替えます。
 *
 * **`/threads/{id}` そのものとは別のルートです。** `mockThread` の正規表現は
 * `/threads/1` で終わるものだけに一致するので、こちらが横取りすることはありません。
 */
export async function mockComments(
  page: Page,
  handler: (route: Route, cursor: string | null) => Promise<unknown>,
) {
  await page.route(/\/threads\/\d+\/comments(\?|$)/, (route) =>
    handler(route, cursorOf(route.request().url())),
  );
}

/** 201 (作成成功) を返します。 */
export function created(route: Route, body: unknown) {
  return route.fulfill({
    status: 201,
    contentType: 'application/json',
    body: JSON.stringify(body),
  });
}

/** 204 (本文なし) を返します。削除の応答。 */
export function noContent(route: Route) {
  return route.fulfill({ status: 204, body: '' });
}

function json(route: Route, status: number, body: unknown) {
  return route.fulfill({
    status,
    contentType: 'application/json',
    body: JSON.stringify(body),
  });
}

/** 仕様書どおりの形でエラーを返す。**クライアントは `code` で分岐する。** */
export function fail(route: Route, status: number, code: ErrorCode, message = 'テスト用の失敗') {
  return json(route, status, { error: { code, message } });
}

export function ok(route: Route, body: unknown) {
  return json(route, 200, body);
}

/** 指定ミリ秒だけ待つ。**順序を作るために使う。** */
export function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * `GET /me` を差し替えます。
 *
 * `null` を渡すと 401 (未ログイン) になります。
 */
export async function mockMe(page: Page, me: Me | null) {
  await page.route('**/me', (route) =>
    me === null ? fail(route, 401, 'UNAUTHENTICATED', 'ログインが必要です') : ok(route, me),
  );
}

/**
 * `GET /threads/{id}` を差し替えます。既定はどの ID でも 200。
 */
export async function mockThread(
  page: Page,
  handler: (route: Route, id: number) => Promise<unknown> = (route) => ok(route, thread()),
) {
  await page.route(/\/threads\/(\d+)(\?|$)/, (route) => {
    // **画面そのものの取得は素通しする。**
    //
    // 詳細画面の URL は `/threads/100` で、**API の宛先と同じ形**になる ——
    // 検査では API を同一オリジンに置いているため (ADR 0020 決定 3)、
    // 区別が付くのはメソッドや要求の種類だけになる。
    // ここで JSON を返すと画面が開かず、原因の分かりにくい失敗になる。
    //
    // `RSC` ヘッダは Next.js のクライアント遷移とプリフェッチが付ける。
    // こちらも画面の取得なので、同じく素通しする。
    const request = route.request();
    if (request.isNavigationRequest() || request.headers()['rsc'] !== undefined) {
      return route.fallback();
    }

    const m = /\/threads\/(\d+)/.exec(request.url());
    return handler(route, Number(m?.[1] ?? 0));
  });
}

/** 通報キューの取得で使われた `status` を取り出します。 */
export function statusOf(url: string): string {
  return new URL(url).searchParams.get('status') ?? '';
}

/** 通報キューの取得で使われた `cursor` を取り出します。 */
export function cursorOf(url: string): string | null {
  return new URL(url).searchParams.get('cursor');
}
