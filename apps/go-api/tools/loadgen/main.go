// Command loadgen は HTTP の読み取り経路に一定の並列度で負荷をかけ、
// スループットとレイテンシの分布を JSON で出力します (Phase 4)。
//
// 【なぜ自前で書くか】
// ADR 0009 の見積もり (読み取りピーク 4,200 RPS) に対する実測が
// README の Phase 4 で「未測定」のまま残っていた。理由は
// 「単一マシンの compose では負荷生成側が先に飽和する」だった。
//
// **先に飽和するなら、それを数字で示せばよい。** そのためには
// 負荷生成側の天井を別途測る必要があり、外部ツールだと
// 「その天井がツールのものか環境のものか」を切り分けられない。
//
// 【コンテナの中で走らせる】
// ホストから叩くと Docker のポート転送が挟まり、macOS では
// そこが最初の律速になる。go-api コンテナには Go の一式が入っているので
// (compose の dev ターゲット)、その中で走らせて localhost を叩く。
//
//	docker compose exec -T go-api go build -o /tmp/loadgen ./tools/loadgen
//	docker compose exec -T go-api /tmp/loadgen -url http://localhost:8080/threads -c 32
//
// 通常は .github/scripts/scale-probe.py が上の 2 つを回します。
//
// **本番バイナリには入りません。** cmd/api とは別の main で、
// Dockerfile の本番ターゲットはこのディレクトリを含みません。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// errorBackoff は接続に失敗したときに待つ時間です。
//
// **正常時のスループットには影響しません** (成功した要求は通らない経路)。
// 短すぎると空回りが止まらず、長すぎると「本当に落ちている」ことの
// 検出が遅れます。
const errorBackoff = 10 * time.Millisecond

func main() {
	var (
		url         = flag.String("url", "http://localhost:8080/healthz", "叩く URL")
		concurrency = flag.Int("c", 8, "並列数")
		duration    = flag.Duration("d", 5*time.Second, "計測時間")
		warmup      = flag.Duration("warmup", 1*time.Second, "計測前に捨てる時間")
	)
	flag.Parse()

	if *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "-c は 1 以上である必要があります")
		os.Exit(2)
	}

	result := run(*url, *concurrency, *duration, *warmup)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "出力に失敗しました: %v\n", err)
		os.Exit(1)
	}
}

// Result は 1 条件ぶんの測定結果です。
type Result struct {
	URL         string  `json:"url"`
	Concurrency int     `json:"concurrency"`
	Seconds     float64 `json:"seconds"`
	Requests    int64   `json:"requests"`
	RPS         float64 `json:"rps"`
	// Errors は接続・読み取りの失敗です。**2xx 以外とは分けます** ——
	// 「繋がらない」と「繋がったが 500」は原因が違います。
	Errors  int64   `json:"errors"`
	Non2xx  int64   `json:"non2xx"`
	P50Msec float64 `json:"p50_ms"`
	P95Msec float64 `json:"p95_ms"`
	P99Msec float64 `json:"p99_ms"`
	MaxMsec float64 `json:"max_ms"`
}

func run(url string, concurrency int, duration, warmup time.Duration) Result {
	// **接続を並列数ぶん張れるようにする。** 既定の MaxIdleConnsPerHost は 2 で、
	// そのままだと毎回 TCP を張り直し、測っているのが
	// 「サーバの処理能力」ではなく「接続確立の速さ」になります。
	transport := &http.Transport{
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		MaxConnsPerHost:     concurrency * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	var (
		requests atomic.Int64
		errors   atomic.Int64
		non2xx   atomic.Int64

		mu      sync.Mutex
		samples []float64

		measuring atomic.Bool
	)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// レイテンシは goroutine ごとに溜めて最後に 1 回だけ束ねる。
			// 毎回ロックを取ると、測定器そのものが律速になります。
			local := make([]float64, 0, 4096)

			// **リクエストは 1 回だけ組み立てて使い回す。** 毎回組み立てると
			// URL の解析ぶんが測定値に混ざる。
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "URL が不正です: %v\n", err)
				os.Exit(2)
			}

			for {
				select {
				case <-stop:
					mu.Lock()
					samples = append(samples, local...)
					mu.Unlock()
					return
				default:
				}

				started := time.Now()
				resp, err := client.Do(req)
				elapsed := time.Since(started)

				// 準備運転の間は数えない。**JIT ではなく接続確立と
				// ページキャッシュのため**で、1 回目だけ極端に遅い。
				counting := measuring.Load()

				if err != nil {
					if counting {
						errors.Add(1)
					}
					// **少しだけ待つ。** API が落ちている・接続を拒否している場合
					// client.Do は即座に返るので、待たないと計測時間いっぱい
					// フルスピードで空回りする。errors が桁違いに膨らむうえ、
					// **loadgen は go-api コンテナの中で動く**ため、
					// 測定対象から CPU を奪う。
					time.Sleep(errorBackoff)
					continue
				}
				// **本文を最後まで読んで閉じる。** 読み捨てないと
				// 接続が再利用されず、並列数を上げるほど TIME_WAIT が積み上がります。
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				if !counting {
					continue
				}
				requests.Add(1)
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					non2xx.Add(1)
				}
				local = append(local, float64(elapsed.Microseconds())/1000.0)
			}
		}()
	}

	time.Sleep(warmup)
	measuring.Store(true)
	started := time.Now()
	time.Sleep(duration)
	measuring.Store(false)
	elapsed := time.Since(started)
	close(stop)
	wg.Wait()

	sort.Float64s(samples)

	return Result{
		URL:         url,
		Concurrency: concurrency,
		Seconds:     elapsed.Seconds(),
		Requests:    requests.Load(),
		RPS:         float64(requests.Load()) / elapsed.Seconds(),
		Errors:      errors.Load(),
		Non2xx:      non2xx.Load(),
		P50Msec:     percentile(samples, 0.50),
		P95Msec:     percentile(samples, 0.95),
		P99Msec:     percentile(samples, 0.99),
		MaxMsec:     percentile(samples, 1.0),
	}
}

// percentile は昇順に並んだ標本から分位点を返します。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}
