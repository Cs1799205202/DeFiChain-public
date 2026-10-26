// Command mockllm is a stand-in for an OpenAI-compatible LLM server, so you can develop and
// test an LLM agent without an API key, offline, for free. It answers every chat request
// with a random but well-formed trading plan in the format the example agents use:
//
//	{"actions": [{"action": "buy_weth", "amount_usd": 1200}], "reason": "mock decision"}
//
//	go run ./cmd/mockllm                      # listens on 127.0.0.1:28199
//	export LLM_BASE_URL=http://127.0.0.1:28199/v1 LLM_MODEL=mock LLM_API_KEY=none
//
// It does not read your prompt: it tests your plumbing (prompting, parsing, validation,
// execution, logging), not your strategy.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:28199", "address to listen on")
	seed := flag.Int64("seed", 1, "random seed")
	latency := flag.Duration("latency", 300*time.Millisecond, "simulated response time")
	holdProb := flag.Float64("hold", 0.4, "probability of answering with an empty plan (hold)")
	garbage := flag.Float64("garbage", 0.05, "probability of answering with invalid JSON, to test your error handling")
	flag.Parse()

	var mu sync.Mutex
	rnd := rand.New(rand.NewSource(*seed))
	count := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "mock", "object": "model"}}}) //nolint:errcheck
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		count++
		n := count
		roll, kind := rnd.Float64(), rnd.Intn(6)
		amount := 500 + rnd.Float64()*4500
		mu.Unlock()
		time.Sleep(*latency)
		var content string
		switch {
		case roll < *garbage:
			content = "Sure! I think you should buy some WETH, it is going up."
		case roll < *garbage+*holdProb:
			content = `{"actions": [], "reason": "mock: hold"}`
		default:
			actions := []string{
				fmt.Sprintf(`{"action": "buy_weth", "amount_usd": %.0f}`, amount),
				fmt.Sprintf(`{"action": "sell_weth", "amount_weth": %.3f}`, amount/2000),
				fmt.Sprintf(`{"action": "deposit", "amount_weth": %.3f}`, amount/2000),
				fmt.Sprintf(`{"action": "borrow", "amount_usd": %.0f}`, amount/2),
				fmt.Sprintf(`{"action": "repay", "amount_usd": %.0f}`, amount/2),
				fmt.Sprintf(`{"action": "withdraw", "amount_weth": %.3f}`, amount/4000),
			}
			content = fmt.Sprintf(`{"actions": [%s], "reason": "mock decision #%d"}`, actions[kind], n)
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"id": fmt.Sprintf("mock-%d", n), "object": "chat.completion", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": content}}},
			"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
	})
	log.Printf("mock LLM on http://%s/v1 (model name: anything)", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
