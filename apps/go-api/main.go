package goapi
package main

import (
    "encoding/json"
    "log"
    "net/http"
)

type Thread struct {
    ID            string `json:"id"`
    Title         string `json:"title"`
    CommentsCount int    `json:"commentsCount"`
}

func main() {
    http.HandleFunc("/threads", func(w http.ResponseWriter, r *http.Request) {
        threads := []Thread{
            {ID: "1", Title: "First thread", CommentsCount: 3},
            {ID: "2", Title: "Second thread", CommentsCount: 5},
        }
        w.Header().Set("Content-Type", "application/json")
        if err := json.NewEncoder(w).Encode(threads); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
        }
    })

    log.Println("Starting Go API on :8080")
    if err := http.ListenAndServe(":8080", nil); err != nil {
        log.Fatal(err)
    }
}
