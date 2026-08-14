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
	// **アイコンの詰め替えを忘れない** (コメントの画像で一度落とした)。
	var icon *oapigen.Image
	if d.Icon != nil {
		icon = &oapigen.Image{
			Id:     d.Icon.ID,
			Url:    d.Icon.URL,
			Width:  d.Icon.Width,
			Height: d.Icon.Height,
		}
	}

	return oapigen.Thread{
		Id:           d.ID,
		Title:        d.Title,
		CommentCount: d.CommentCount,
		Author:       author,
		Icon:         icon,
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
		// ドメインの Role と仕様書の enum は同じ 3 値。
		// 増やすときは DB の CHECK 制約も含めて 3 か所を揃えること。
		Role: oapigen.Role(d.Role),
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
	// **画像の詰め替えを忘れない。**
	// DTO に載っていても、ここで写さなければ応答に現れない。
	// 初版はこれを落としており、ユニットテストは
	// 「リポジトリに image_id が渡ったか」しか見ていなかったので通っていた。
	// 実 DB を通すスモークで初めて落ちた。
	var img *oapigen.Image
	if d.Image != nil {
		img = &oapigen.Image{
			Id:     d.Image.ID,
			Url:    d.Image.URL,
			Width:  d.Image.Width,
			Height: d.Image.Height,
		}
	}

	return oapigen.Comment{
		Id:         d.ID,
		ThreadId:   d.ThreadID,
		Seq:        d.Seq,
		AuthorName: d.AuthorName,
		Author:     author,
		Image:      img,
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
