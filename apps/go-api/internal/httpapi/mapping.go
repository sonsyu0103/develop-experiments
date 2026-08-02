package httpapi

import (
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
)

// ユースケース層の DTO を、仕様書から生成されたワイヤ型へ変換します。
//
// ユースケース層の DTO をそのまま JSON にせず、ここで詰め替えているのは、
// レスポンスの形が api/openapi.yaml と一致することを
// コンパイル時に保証するためです。
// 仕様からフィールドを消すと、この関数がコンパイルエラーになります。

func toWireThread(d threadusecase.ThreadDTO) oapigen.Thread {
	return oapigen.Thread{
		Id:           d.ID,
		Title:        d.Title,
		CommentCount: d.CommentCount,
		CreatedAt:    d.CreatedAt,
	}
}

func toWireThreadList(r threadusecase.ThreadListResult) oapigen.ThreadList {
	threads := make([]oapigen.Thread, 0, len(r.Threads))
	for _, d := range r.Threads {
		threads = append(threads, toWireThread(d))
	}
	return oapigen.ThreadList{
		Threads:    threads,
		NextCursor: r.NextCursor,
	}
}

func toWireComment(d commentusecase.CommentDTO) oapigen.Comment {
	return oapigen.Comment{
		Id:         d.ID,
		ThreadId:   d.ThreadID,
		AuthorName: d.AuthorName,
		Body:       d.Body,
		CreatedAt:  d.CreatedAt,
	}
}

func toWireCommentList(r commentusecase.CommentListResult) oapigen.CommentList {
	comments := make([]oapigen.Comment, 0, len(r.Comments))
	for _, d := range r.Comments {
		comments = append(comments, toWireComment(d))
	}
	return oapigen.CommentList{
		Comments:   comments,
		NextCursor: r.NextCursor,
	}
}
