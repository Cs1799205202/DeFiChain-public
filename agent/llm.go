// Package agent contains the trading agents: scripted bots (arbitrage, liquidation,
// sandwich) and an LLM trader that decides from market observations.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LLM is a minimal OpenAI-compatible chat client.
type LLM struct {
	BaseURL string
	APIKey  string
	Model   string
	// Thinking lets reasoning models think before answering. Off by default: deepseek-v4
	// with thinking often spends the whole token budget reasoning and returns no answer,
	// and a trading round should not wait a minute for one.
	Thinking  bool
	MaxTokens int
	http      *http.Client
	plain     bool // send only the standard fields (set after a server rejected the others)
}

// NewLLMFromEnv reads LLM_BASE_URL, LLM_MODEL and LLM_API_KEY (or LLM_API_KEY_FILE). The
// defaults point at cmd/mockllm, the offline stand-in: set the three variables to use a
// real model (any OpenAI-compatible endpoint).
func NewLLMFromEnv() (*LLM, error) {
	l := &LLM{
		BaseURL:  getenv("LLM_BASE_URL", "http://127.0.0.1:28199/v1"),
		Model:    getenv("LLM_MODEL", "mock"),
		APIKey:   os.Getenv("LLM_API_KEY"),
		Thinking: os.Getenv("LLM_THINKING") == "on",
		http:     &http.Client{Timeout: 120 * time.Second},
	}
	l.MaxTokens = 1000
	if l.Thinking {
		l.MaxTokens = 16000
		l.http.Timeout = 10 * time.Minute // thinking can take minutes on a busy server
	}
	if l.APIKey == "" {
		if f := os.Getenv("LLM_API_KEY_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("read LLM_API_KEY_FILE: %w", err)
			}
			l.APIKey = strings.TrimSpace(string(b))
		}
	}
	if l.APIKey == "" && !strings.Contains(l.BaseURL, "127.0.0.1") && !strings.Contains(l.BaseURL, "localhost") {
		return nil, fmt.Errorf("set LLM_API_KEY or LLM_API_KEY_FILE for %s", l.BaseURL)
	}
	return l, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatJSON sends a system and user prompt and decodes the JSON reply into out. Rate-limit
// and overload errors (429/5xx) are retried with backoff. If a thinking model uses up its
// budget before answering, the call is repeated once with thinking off.
func (l *LLM) ChatJSON(ctx context.Context, system, user string, out any) (string, error) {
	thinking := l.Thinking
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		content, finish, status, err := l.chat(ctx, system, user, thinking)
		if err != nil && status == http.StatusBadRequest && !l.plain {
			// Some servers reject the optional fields (JSON mode, chat_template_kwargs).
			l.plain = true
			lastErr = err
			continue
		}
		switch {
		case err == nil && content == "" && finish == "length" && thinking:
			lastErr = fmt.Errorf("llm: ran out of tokens while thinking; retrying without thinking")
			thinking = false
			continue
		case err == nil && content == "":
			lastErr = fmt.Errorf("llm: empty reply (finish=%s)", finish)
			continue
		case err != nil && (status == http.StatusTooManyRequests || status >= 500):
			lastErr = err
			continue
		case err != nil:
			return "", err
		}
		content = extractJSON(content)
		if err := json.Unmarshal([]byte(content), out); err != nil {
			lastErr = fmt.Errorf("llm: decode %q: %w", truncate(content, 120), err)
			continue
		}
		return content, nil
	}
	return "", lastErr
}

func (l *LLM) chat(ctx context.Context, system, user string, thinking bool) (content, finish string, status int, err error) {
	req := map[string]any{
		"model":       l.Model,
		"messages":    []chatMessage{{"system", system}, {"user", user}},
		"temperature": 0.3,
		"max_tokens":  l.MaxTokens,
	}
	if !l.plain {
		req["response_format"] = map[string]string{"type": "json_object"}
	}
	if !thinking && !l.plain {
		// vLLM passes these to the chat template; different model families read different keys.
		req["chat_template_kwargs"] = map[string]any{"thinking": false, "enable_thinking": false}
	}
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", 0, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+l.APIKey)
	resp, err := l.http.Do(hreq)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", resp.StatusCode, fmt.Errorf("llm %d: %.200s", resp.StatusCode, raw)
	}
	var cr struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return "", "", resp.StatusCode, fmt.Errorf("llm: bad response: %.200s", raw)
	}
	return cr.Choices[0].Message.Content, cr.Choices[0].FinishReason, resp.StatusCode, nil
}

// extractJSON drops reasoning (<think>...</think>) and a ```json fence, keeping the object.
func extractJSON(s string) string {
	for {
		i := strings.Index(s, "<think>")
		j := strings.Index(s, "</think>")
		if i < 0 || j < i {
			break
		}
		s = s[:i] + s[j+len("</think>"):]
	}
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}
