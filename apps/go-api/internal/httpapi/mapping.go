package httpapi

import (
	commentusecase "develop-experiments/apps/go-api/internal/comment/usecase"
	"develop-experiments/apps/go-api/internal/httpapi/oapigen"
	threadusecase "develop-experiments/apps/go-api/internal/thread/usecase"
	userusecase "develop-experiments/apps/go-api/internal/user/usecase"
)

// ユースケース層の DTO を、仕様書から生成されたワイヤ型へ変換します。
//
// ユースケース層の DTO をそのまま JSON にせず、ここで詰め替えているのは、
// レスポンスの形が api/openapi.yaml と一致することを
// コンパイル時に保証するためです。
// 仕様からフィールドを消すと、この関数がコンパイルエラーになります。

func toWireThread(d threadusecase.ThreadDTO) oapigen.Thread {
	var author *oapigen.Author
	if d.Author != nil {
		author = &oapigen.Author{
			PublicId:    d.Author.PublicID,
			DisplayName: d.Author.DisplayName,
			AvatarUrl:   d.Author.AvatarURL,
			Withdrawn:   d.Author.Withdrawn,
		}
	}
	return oapigen.Thread{
		Id:           d.ID,
		Title:        d.Title,
		CommentCount: d.CommentCount,
		Author:       author,
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

// toWireMe はログイン中の利用者をワイヤ型へ変換します。
//
// 内部 ID を含めないことは仕様書側でも保証されています
// (Me スキーマに id が無いため、書こうとしてもコンパイルが通りません)。
func toWireMe(d userusecase.MeDTO) oapigen.Me {
	return oapigen.Me{
		PublicId:    d.PublicID,
		DisplayName: d.DisplayName,
		Email:       d.Email,
		AvatarUrl:   d.AvatarURL,
	}
}

func toWireComment(d commentusecase.CommentDTO) oapigen.Comment {
	var author *oapigen.Author
	if d.Author != nil {
		author = &oapigen.Author{
			PublicId:    d.Author.PublicID,
			DisplayName: d.Author.DisplayName,
			AvatarUrl:   d.Author.AvatarURL,
			Withdrawn:   d.Author.Withdrawn,
		}
	}
	return oapigen.Comment{
		Id:         d.ID,
		ThreadId:   d.ThreadID,
		AuthorName: d.AuthorName,
		Author:     author,
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
