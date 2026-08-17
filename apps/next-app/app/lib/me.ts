'use client';

// ログイン状態をブラウザ側で持つ。
//
// 【なぜ 1 か所にまとめたか】
// 画面が増えて、`GET /me` を見る部品が 1 ページに 2 つ以上並ぶようになった
// (ヘッダの管理リンクと、画面本体の出し分け)。それぞれが独立に叩くと、
//
//   - **同じ応答を 2 回取りに行く**
//   - **片方だけが古くなる。** ログアウトはマイページの操作だが、
//     ヘッダの「管理」リンクも同時に消える必要がある ——
//     部品ごとに状態を持つと、ログアウト後もヘッダだけログイン中のまま残る
//
// 後者のために、控えを更新したら**購読している部品すべてに知らせる。**
//
// 【この控えは利用者をまたがない】
// 保持しているのはタブの中のメモリで、**タブは 1 人のものになる。**
// ADR 0005 が禁じているのは、サーバや CDN のように
// **利用者をまたいで共有される場所**に認証済みの応答を置くことで、
// `fetch` 自体は `cache: 'no-store'` のまま (api.ts)。
import { useCallback, useEffect, useState } from 'react';

import { ApiError, getMe, type Me } from './api';
import { describe } from './errors';

export type MeState =
  | { kind: 'loading' }
  | { kind: 'anonymous' }
  | { kind: 'ready'; me: Me }
  | { kind: 'error'; message: string };

/**
 * 取得中・取得済みの結果。
 *
 * **失敗は保持しない** (下の `catch` で捨てる) —— 一過性の失敗を握ると、
 * 以後このタブでは二度とログイン状態を確認できなくなる。
 */
let inflight: Promise<Me | null> | null = null;

/** 控えが変わったことを知りたい部品。**マウント中のものだけが入る。** */
const listeners = new Set<() => void>();

function notify(): void {
  listeners.forEach((listener) => {
    listener();
  });
}

/**
 * ログイン状態を引く。**取得済みなら同じ結果を返す。**
 *
 * フックを使えない場所 (効果の中で 1 度だけ読みたい場合など) 向け。
 * 未ログインは `null` で、例外にはならない。
 */
export function loadMe(): Promise<Me | null> {
  inflight ??= getMe()
    .then((me): Me | null => me)
    .catch((e: unknown) => {
      // **401 は失敗ではない。** 未ログインという既定の状態なので、
      // `null` として控える (毎回問い合わせ直す必要はない)。
      if (e instanceof ApiError && e.code === 'UNAUTHENTICATED') {
        return null;
      }
      inflight = null;
      throw e;
    });

  return inflight;
}

/**
 * 控えを捨てて、購読している部品に引き直させる。
 *
 * **ログアウトの直後に必ず呼ぶ。** 忘れると、Cookie は消えているのに
 * 画面だけログイン中のまま残る。
 */
export function invalidateMe(): void {
  inflight = null;
  notify();
}

/**
 * 手元にある `Me` で控えを置き換える。
 *
 * アバターの変更のように、**応答が新しい `Me` をそのまま返す**場合に、
 * もう一往復させないために使う。こちらも購読側へ知らせるので、
 * ヘッダなど別の場所の表示も同時に変わる。
 */
export function primeMe(me: Me): void {
  inflight = Promise.resolve(me);
  notify();
}

/**
 * ログイン状態を読む。
 *
 * `reload()` は控えを捨てて引き直す。**取得に失敗したときの再試行**が主な用途で、
 * ログアウトやアバターの変更では `invalidateMe` / `primeMe` のほうを使う
 * (そちらは他の部品にも伝わる)。
 */
export function useMe(): { state: MeState; reload: () => void } {
  const [state, setState] = useState<MeState>({ kind: 'loading' });
  // 控えが変わるたびに進む番号。効果の依存に入れて、取得をやり直させる。
  const [generation, setGeneration] = useState(0);

  useEffect(() => {
    const onChange = () => {
      setGeneration((n) => n + 1);
    };
    listeners.add(onChange);
    return () => {
      listeners.delete(onChange);
    };
  }, []);

  useEffect(() => {
    // **画面を離れたあとに setState しない。**
    // 開発時の Strict Mode では効果が 2 回走るため、
    // 片付けないと解決済みの古い応答で状態が上書きされる。
    let alive = true;

    // **ここで `loading` に戻さない。** 効果の中で同期的に setState すると
    // 連鎖描画になる (react-hooks/set-state-in-effect)。初期値が `loading` で、
    // 引き直しの合図はイベントハンドラ側にある。
    loadMe()
      .then((me) => {
        if (!alive) return;
        setState(me === null ? { kind: 'anonymous' } : { kind: 'ready', me });
      })
      .catch((e: unknown) => {
        if (!alive) return;
        // **401 以外を未ログイン扱いにしない** (ADR 0013 決定 3)。
        // 障害中にログイン画面へ送り続けると、原因から遠ざかる。
        setState({ kind: 'error', message: describe(e) });
      });

    return () => {
      alive = false;
    };
  }, [generation]);

  const reload = useCallback(() => {
    setState({ kind: 'loading' });
    // 控えを捨てると `notify` が走り、この部品の generation も進む
    // —— 取得のやり直しはそちらの効果が行う。
    invalidateMe();
  }, []);

  return { state, reload };
}
