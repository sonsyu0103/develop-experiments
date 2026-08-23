// 通報キューの状態遷移。
//
// **この 4 本が、この枠組みを入れた理由そのものです。**
// レビューで出た指摘 ①〜④ を、それぞれ 1 本ずつ再現しています ——
// どれも `tsc` / `eslint` / `next build` では捕まらず、
// 実バックエンドでも作れない状態になります
// (「A の応答が B より後に届く」を再現できないため)。
import { expect, test, type Route } from '@playwright/test';

import {
  cursorOf,
  delay,
  fail,
  list,
  mockMe,
  mockThread,
  moderator,
  ok,
  report,
  statusOf,
  thread,
} from './api-mock';

test.beforeEach(async ({ page }) => {
  await mockMe(page, moderator);
});

// ---------------------------------------------------------------------------
// 指摘 ①: 古い応答が新しい一覧に混ざる
// ---------------------------------------------------------------------------

test('絞り込みを変えたら、遅れて届いた古い応答を捨てる', async ({ page }) => {
  await mockThread(page);

  // **open だけを遅らせます。** これで「絞り込みを変えたあとに
  // 古い応答が届く」順序を作れます。実バックエンドでは作れません。
  await page.route('**/moderation/reports*', async (route: Route) => {
    const status = statusOf(route.request().url());
    if (status === 'open') {
      await delay(1500);
      return ok(route, list([report({ id: 11, targetId: 111 })], 'cursor-open'));
    }
    return ok(route, list([report({ id: 22, targetId: 222, status: 'resolved' })]));
  });

  await page.goto('/admin');

  // 最初の取得 (open) が返る前に絞り込みを変える。
  await page.getByLabel('絞り込み').selectOption('resolved');

  await expect(page.getByText('#22 /')).toBeVisible();

  // **遅れて届いた open の行が現れないこと。**
  // 現れると、見出しは「対処した」なのに未処理の行が並びます。
  await page.waitForTimeout(2500);
  await expect(page.getByText('#11 /')).toHaveCount(0);
  await expect(page.getByText('#22 /')).toBeVisible();

  // **カーソルも古いクエリのもので上書きされないこと。**
  // resolved の応答は nextCursor が null なので、次ページの導線は出ません。
  await expect(page.getByRole('button', { name: '次のページ' })).toHaveCount(0);
});

// ---------------------------------------------------------------------------
// 指摘 ②: 行をまたぐと二重送信できる
// ---------------------------------------------------------------------------

test('別の行の処理が終わっても、処理中の行のボタンは押せないままにする', async ({ page }) => {
  await mockThread(page);
  await page.route('**/moderation/reports*', (route) =>
    ok(route, list([report({ id: 1, targetId: 101 }), report({ id: 2, targetId: 102 })])),
  );

  // 1 件目は速く、2 件目は遅く返す。
  // **速いほうが終わった時点で、遅いほうのボタンが復活しないこと**を見ます。
  await page.route('**/moderation/actions', async (route: Route) => {
    const body = route.request().postDataJSON() as { targetId: string };
    if (body.targetId === '102') {
      await delay(2500);
    }
    return ok(route, {
      id: 1,
      action: 'delete_thread',
      targetType: 'thread',
      targetId: body.targetId,
      reason: null,
      createdAt: '2026-08-16T00:00:00Z',
    });
  });

  await page.goto('/admin');

  const rows = page.getByRole('listitem');
  const slowDelete = rows.filter({ hasText: '#2 /' }).getByRole('button', { name: '対象を削除' });
  const fastDelete = rows.filter({ hasText: '#1 /' }).getByRole('button', { name: '対象を削除' });

  // 遅いほうを先に押し、そのあと速いほうを押す。
  await slowDelete.click();
  await fastDelete.click();

  // 速いほうが終わる。
  await expect(rows.filter({ hasText: '#1 /' }).getByText('削除しました')).toBeVisible();

  // **遅いほうはまだ処理中なので、押せてはいけません。**
  // 単一の ID で持っていると、ここで復活して削除の二重送信が通ります。
  await expect(slowDelete).toBeDisabled();

  // 終われば戻る。
  await expect(rows.filter({ hasText: '#2 /' }).getByText('削除しました')).toBeVisible({
    timeout: 5000,
  });
  await expect(slowDelete).toBeEnabled();
});

// ---------------------------------------------------------------------------
// 指摘 ③: 一過性の取得失敗が「読み込み中」で固定される
// ---------------------------------------------------------------------------

test('対象スレッドの取得が 500 なら、読み込み中のままにしない', async ({ page }) => {
  await page.route('**/moderation/reports*', (route) =>
    ok(route, list([report({ id: 1, targetId: 100 })])),
  );
  await mockThread(page, (route) => fail(route, 500, 'INTERNAL'));

  await page.goto('/admin');

  await expect(page.getByText('対象のスレッドを取得できませんでした')).toBeVisible();
  // **再取得の導線が無いので、ここが「取得しています...」だと
  // 永久に読み込み中に見えます。**
  await expect(page.getByText('対象のスレッドを取得しています...')).toHaveCount(0);
});

// **スレッド ID を持たないコメント通報も、読み込み中で固定しない** (レビュー指摘)。
//
// hydrateThreads は threadId が無い行を除外するので、その行の取得は
// **永久に始まりません。** 指摘 ③ と同じ「読み込み中に見え続ける」形が、
// この経路にだけ残っていました (onDelete 側は検出していたのに表示側だけ抜け)。
//
// reports.target_thread_id はコメントの通報に必須 (CHECK 制約) なので
// 本来は起こらない状態ですが、**起こらないはずの状態こそ画面に出す**
// —— 出さないと、壊れていることが「まだ読み込み中」に見えます。
test('スレッド ID の無いコメント通報を、読み込み中のままにしない', async ({ page }) => {
  await page.route('**/moderation/reports*', (route) =>
    ok(route, list([report({ id: 1, targetType: 'comment', targetId: 500 })])),
  );

  await page.goto('/admin');

  await expect(page.getByText('対象のスレッドが記録されていません')).toBeVisible();
  await expect(page.getByText('対象のスレッドを取得しています...')).toHaveCount(0);
});

test('対象スレッドが 404 なら「既に削除されています」と出す', async ({ page }) => {
  await page.route('**/moderation/reports*', (route) =>
    ok(route, list([report({ id: 1, targetId: 100 })])),
  );
  await mockThread(page, (route) => fail(route, 404, 'NOT_FOUND'));

  await page.goto('/admin');

  // **404 は情報です。** 失敗と同じ扱いにしてはいけません。
  await expect(page.getByText('対象のスレッドは既に削除されています')).toBeVisible();
});

// ---------------------------------------------------------------------------
// 指摘 ④: 失敗と「通報はありません」が同時に出る
// ---------------------------------------------------------------------------

test('キューの取得に失敗したら「通報はありません」を出さない', async ({ page }) => {
  await page.route('**/moderation/reports*', (route) => fail(route, 500, 'INTERNAL'));

  await page.goto('/admin');

  await expect(page.getByText('INTERNAL')).toBeVisible();
  // **API が落ちているのに「未処理は無い」と読める表示**にしないこと。
  await expect(page.getByText('この状態の通報はありません。')).toHaveCount(0);
});

test('本当に 0 件なら「通報はありません」を出す', async ({ page }) => {
  await page.route('**/moderation/reports*', (route) => ok(route, list([])));

  await page.goto('/admin');

  // ④ の対処で「常に出さない」にしてしまうと、
  // **0 件と障害の区別が今度は逆向きに消えます。**
  await expect(page.getByText('この状態の通報はありません。')).toBeVisible();
});

// ---------------------------------------------------------------------------
// キュー固有の踏みやすい形
// ---------------------------------------------------------------------------

test('コメントの通報の削除には threadId を必ず添える', async ({ page }) => {
  await page.route('**/moderation/reports*', (route) =>
    ok(route, list([report({ id: 1, targetType: 'comment', targetId: 500, threadId: 7 })])),
  );
  await mockThread(page, (route, id) => ok(route, thread({ id, title: 'コメントの親' })));

  let sent: Record<string, unknown> | null = null;
  await page.route('**/moderation/actions', (route) => {
    sent = route.request().postDataJSON() as Record<string, unknown>;
    return ok(route, {
      id: 1,
      action: 'delete_comment',
      targetType: 'comment',
      targetId: '7:500',
      reason: null,
      createdAt: '2026-08-16T00:00:00Z',
    });
  });

  await page.goto('/admin');
  await page.getByRole('button', { name: '対象を削除' }).click();
  await expect(page.getByText('削除しました')).toBeVisible();

  // **threadId が無いと 8 パーティション全走査になり、
  // しかも記録から対象を引けなくなります** (ADR 0011)。
  expect(sent).toEqual({ action: 'delete_comment', targetId: '500', threadId: 7 });
});

test('次のページは受け取ったカーソルをそのまま渡す', async ({ page }) => {
  await mockThread(page);

  const seen: (string | null)[] = [];
  await page.route('**/moderation/reports*', (route) => {
    const cursor = cursorOf(route.request().url());
    seen.push(cursor);
    return cursor === null
      ? ok(route, list([report({ id: 1, targetId: 101 })], 'opaque-token-1'))
      : ok(route, list([report({ id: 2, targetId: 102 })]));
  });

  await page.goto('/admin');
  await page.getByRole('button', { name: '次のページ' }).click();

  // **古い順に積み上げる。** キューは他の一覧と向きが逆なので、
  // 追記ではなく置換にすると、処理の順序が飛びます。
  await expect(page.getByText('#1 /')).toBeVisible();
  await expect(page.getByText('#2 /')).toBeVisible();

  // カーソルは不透明トークン。**解釈も生成も改変もしない** (ADR 0018)。
  expect(seen).toEqual([null, 'opaque-token-1']);
});

test('処理済みの通報では「対処した」を押せない', async ({ page }) => {
  await mockThread(page);
  await page.route('**/moderation/reports*', (route) =>
    ok(route, list([report({ id: 1, targetId: 101, status: 'resolved', resolvedAt: '2026-08-16T01:00:00Z' })])),
  );

  await page.goto('/admin');

  await expect(page.getByRole('button', { name: '対処した' })).toBeDisabled();
  await expect(page.getByRole('button', { name: '対処不要' })).toBeDisabled();
  // **削除は別の判断**なので、こちらは押せたままにします。
  await expect(page.getByRole('button', { name: '対象を削除' })).toBeEnabled();
});
