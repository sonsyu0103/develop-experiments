package main

import (
	"fmt"
	"os"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

func main() {
	// 1. 基本情報を定義する。
	swagger := &openapi3.T{
		OpenAPI: "3.0.3",
		Info: &openapi3.Info{
			Title:       "Develop Experiments API",
			Version:     "1.0",
			Description: "巨大事業のための、並列処理を組み込んだ掲示板API",
		},
		Paths: &openapi3.Paths{},
		Components: &openapi3.Components{
			Schemas: openapi3.Schemas{},
		},
	}

	// 2. ThreadDTO のスキーマを構築
	idSchema := openapi3.NewIntegerSchema()
	idSchema.Example = 1

	titleSchema := openapi3.NewStringSchema()
	titleSchema.Example = "並列処理を学ぶ部屋"

	countSchema := openapi3.NewIntegerSchema()
	countSchema.Example = 7

	threadSchema := openapi3.NewSchemaRef("", openapi3.NewObjectSchema())
	threadSchema.Value.Properties = openapi3.Schemas{
		"id":           openapi3.NewSchemaRef("", idSchema),
		"title":        openapi3.NewSchemaRef("", titleSchema),
		"commentCount": openapi3.NewSchemaRef("", countSchema),
	}

	swagger.Components.Schemas["ThreadDTO"] = threadSchema

	// 3. レスポンスの構造を定義
	responseContent := openapi3.NewContentWithJSONSchema(
		openapi3.NewObjectSchema().WithProperty("threads", openapi3.NewArraySchema().WithItems(threadSchema.Value)),
	)

	// Responses を正しく初期化して、安全に "200" のキーをセットする
	responses := openapi3.NewResponses()
	responses.Set("200", &openapi3.ResponseRef{
		Value: &openapi3.Response{
			Description: pointerToString("成功"),
			Content:     responseContent,
		},
	})

	// パス（エンドポイント）を登録
	swagger.Paths.Set("/threads", &openapi3.PathItem{
		Get: &openapi3.Operation{
			Summary:     "スレッド一覧の取得",
			Description: "並列処理でコメント数を集計したスレッド一覧を返します",
			Responses:   responses, // 修正した変数をここに渡す
		},
	})

	// 4. シリアライズして書き出す
	yamlNode, err := swagger.MarshalYAML()
	if err != nil {
		panic(err)
	}

	bytes, err := yaml.Marshal(yamlNode)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile("openapi.yaml", bytes, 0644)
	if err != nil {
		panic(err)
	}

	fmt.Println("openapi.yaml (OpenAPI 3.0) を正常に生成したよ。")
}

func pointerToString(s string) *string {
	return &s
}
