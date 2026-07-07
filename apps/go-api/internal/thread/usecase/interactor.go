package usecase
package usecase

import (
	"sync"
	"github.com/example/go-api/internal/thread/domain/model"
)

// Aggregator collects counts from multiple sources concurrently.
func AggregateThreads(threads []model.Thread) int {
	var wg sync.WaitGroup
	countCh := make(chan int, len(threads))

	for _, t := range threads {
		wg.Add(1)
		go func(th model.Thread) {
			defer wg.Done()
			// simulate work; here we just send the comments count
			countCh <- th.CommentsCount
		}(t)
	}

	wg.Wait()
	close(countCh)
	total := 0
	for c := range countCh {
		total += c
	}
	return total
}
