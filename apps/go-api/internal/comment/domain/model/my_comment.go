package model

import "time"

// MyComment はマイページに出す「自分が書いたコメント」です。
//
// **Comment と分けているのは、必要な情報が逆向きだからです。**
// スレッド内の一覧では投稿者が要りますがスレッドは自明で、
// こちらでは投稿者が自明でスレッドが要ります。
// Comment に ThreadTitle を足すと、スレッド内の一覧では
// 全行に同じタイトルが載ることになります。
//
// **投稿者 (Author / AuthorName) は持ちません。** 取得の条件が
// 「author_id = 自分」なので、行ごとに持たせても常に同じ値になります。
type MyComment struct {
	ID       int64
	ThreadID int64
	// ThreadTitle は投稿先のスレッドのタイトルです。
	//
	// **削除済みのスレッドのタイトルもそのまま入ります。** 伏せると、
	// 自分が何に書いたのか本人にも分からなくなります。
	ThreadTitle string
	// ThreadDeleted は投稿先のスレッドが論理削除済みかどうかです。
	//
	// **削除済みでもコメントは一覧から落としません。** 落とすと、
	// 自分の投稿が「消えた」のか「元から無い」のかを本人が区別できません。
	// リンク先は 404 になるので、画面側でその旨を出します。
	ThreadDeleted bool
	// Seq はスレッド内のレス番号です (Comment.Seq と同じ意味)。
	Seq int32
	// Image は添付画像です。画像がなければ nil になります。
	Image     *Image
	Body      string
	CreatedAt time.Time
}

// ReconstructMyComment は永続化層が読み出した値からエンティティを組み立てます。
//
// **検証は行いません。** 保存済みの値であり、
// 書き込み時 (NewComment) に検証を通ったものだからです。
func ReconstructMyComment(
	id, threadID int64, threadTitle string, threadDeleted bool,
	seq int32, body string, image *Image, createdAt time.Time,
) *MyComment {
	return &MyComment{
		ID:            id,
		ThreadID:      threadID,
		ThreadTitle:   threadTitle,
		ThreadDeleted: threadDeleted,
		Seq:           seq,
		Body:          body,
		Image:         image,
		CreatedAt:     createdAt,
	}
}
