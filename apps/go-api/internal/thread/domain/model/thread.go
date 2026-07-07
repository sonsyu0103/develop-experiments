package model

import "time"

// Thread は掲示板の「スレッド」そのものを表す構造体です
type Thread struct {
	ID        int       // スレッドを一意に識別する背番号
	Title     string    // スレッドのタイトル
	CreatedAt time.Time // このスレッドが作られた日時
}

// NewThread は、新しいスレッドの「実体」を作って、その「住所」を返す関数です
func NewThread(id int, title string) *Thread {
	return &Thread{
		ID:        id,
		Title:     title,
		CreatedAt: time.Now(), // 今この瞬間の時刻をセット
	}
}
