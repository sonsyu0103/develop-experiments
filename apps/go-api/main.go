package main

import (
	"develop-experiments/apps/go-api/internal/thread/usecase"
	"net/http"

	"github.com/gin-gonic/gin"
)

// @title           Develop Experiments API
// @version         1.0
// @description     巨大事業のための、並列処理を組み込んだ掲示板API
// @host            localhost:8080
// @BasePath        /
func main() {
	r := gin.Default()

	threadUC := &usecase.ThreadInteractor{}

	// @Summary      スレッド一覧の取得
	// @Description  並列処理でコメント数を集計したスレッド一覧を返します
	// @Tags         threads
	// @Accept       json
	// @Produce      json
	// @Success      200  {object}  map[string][]usecase.ThreadDTO
	// @Router       /threads [get]
	r.GET("/threads", func(c *gin.Context) {
		data, err := threadUC.FetchThreadList(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"threads": data})
	})

	r.Run(":8080")
}
