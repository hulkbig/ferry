// perf-lab is an isolated synthetic upstream and load driver; never use production keys.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Scenario struct {
	DelayMS    int `json:"delay_ms"`
	JitterMS   int `json:"jitter_ms"`
	Bytes      int `json:"content_bytes"`
	Chunks     int `json:"chunks"`
	IntervalMS int `json:"interval_ms"`
	Status     int `json:"status"`
	ErrorEvery int `json:"error_every"`
}
type Mock struct {
	mu        sync.RWMutex
	cfg       Scenario
	requests  atomic.Int64
	active    atomic.Int64
	cancelled atomic.Int64
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/config" {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.Method == "POST" {
			var c Scenario
			if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&c) != nil || c.Bytes < 0 || c.Bytes > 4<<20 || c.DelayMS < 0 || c.DelayMS > 60000 || c.JitterMS < 0 || c.JitterMS > 10000 || c.IntervalMS < 0 || c.IntervalMS > 10000 || c.Chunks < 1 || c.Chunks > 10000 || c.ErrorEvery < 0 || (c.Status != 0 && (c.Status < 200 || c.Status > 599)) {
				http.Error(w, "invalid scenario", 400)
				return
			}
			m.cfg = c
		}
		json.NewEncoder(w).Encode(m.cfg)
		return
	}
	if r.URL.Path == "/stats" {
		json.NewEncoder(w).Encode(map[string]int64{"requests": m.requests.Load(), "active": m.active.Load(), "cancelled": m.cancelled.Load()})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/models") {
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","owned_by":"mock"}]}`)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	var input struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&input) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	m.mu.RLock()
	c := m.cfg
	m.mu.RUnlock()
	n := m.requests.Add(1)
	m.active.Add(1)
	defer m.active.Add(-1)
	defer func() {
		if r.Context().Err() != nil {
			m.cancelled.Add(1)
		}
	}()
	delay := c.DelayMS
	if c.JitterMS > 0 {
		delay += int(n % int64(c.JitterMS+1))
	}
	if !sleep(r.Context(), time.Duration(delay)*time.Millisecond) {
		return
	}
	if c.Status >= 400 && (c.ErrorEvery == 0 || n%int64(c.ErrorEvery) == 0) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c.Status)
		io.WriteString(w, `{"error":{"message":"synthetic failure","type":"mock_error"}}`)
		return
	}
	text := strings.Repeat("x", c.Bytes)
	usage := map[string]int{"prompt_tokens": 32, "completion_tokens": max(1, c.Bytes/4), "total_tokens": 32 + max(1, c.Bytes/4)}
	if !input.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-mock", "object": "chat.completion", "created": 1, "model": input.Model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(delta map[string]string, finish any, u any) bool {
		obj := map[string]any{"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": input.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if u != nil {
			obj["usage"] = u
		}
		data, _ := json.Marshal(obj)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		http.NewResponseController(w).Flush()
		return true
	}
	for i := 0; i < c.Chunks; i++ {
		if i > 0 && !sleep(r.Context(), time.Duration(c.IntervalMS)*time.Millisecond) {
			return
		}
		if !send(map[string]string{"content": text[i*c.Bytes/c.Chunks : (i+1)*c.Bytes/c.Chunks]}, nil, nil) {
			return
		}
	}
	if !send(map[string]string{}, "stop", usage) {
		return
	}
	io.WriteString(w, "data: [DONE]\n\n")
	http.NewResponseController(w).Flush()
}

type Sample struct {
	StartMS        float64 `json:"start_ms"`
	LatencyMS      float64 `json:"latency_ms"`
	FirstContentMS float64 `json:"first_content_ms"`
	QueueMS        float64 `json:"queue_ms"`
	MaxGapMS       float64 `json:"max_gap_ms"`
	Bytes          int     `json:"wire_bytes"`
	Content        int     `json:"content_bytes"`
	Status         int     `json:"status"`
	Error          string  `json:"error,omitempty"`
}

func quant(v []float64) map[string]float64 {
	sort.Float64s(v)
	out := map[string]float64{}
	for k, p := range map[string]float64{"p50": .5, "p95": .95, "p99": .99} {
		if len(v) > 0 {
			out[k] = v[min(len(v)-1, int(float64(len(v)-1)*p))]
		}
	}
	return out
}
func load(args []string) {
	f := flag.NewFlagSet("load", flag.ExitOnError)
	url := f.String("url", "", "full chat URL")
	key := f.String("key", "bench-only", "synthetic key")
	duration := f.Duration("duration", 10*time.Second, "arrival window")
	concurrency := f.Int("concurrency", 16, "workers")
	rate := f.Float64("rate", 0, "open-loop requests/sec; 0 is closed-loop saturation")
	stream := f.Bool("stream", false, "SSE")
	prompt := f.Int("prompt-bytes", 128, "prompt bytes")
	expected := f.Int("expected-bytes", 1024, "expected content")
	timeout := f.Duration("timeout", 30*time.Second, "per-request timeout")
	output := f.String("out", "/dev/stdout", "raw result JSON")
	f.Parse(args)
	if *concurrency < 1 || *concurrency > 4096 || *duration <= 0 || *prompt < 0 || *prompt > 4<<20 || *rate < 0 {
		panic("invalid load settings")
	}
	transport := &http.Transport{MaxIdleConns: *concurrency * 2, MaxIdleConnsPerHost: *concurrency, MaxConnsPerHost: *concurrency, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: *timeout}
	body, _ := json.Marshal(map[string]any{"model": "gpt-4o-mini", "messages": []any{map[string]string{"role": "user", "content": strings.Repeat("p", *prompt)}}, "stream": *stream, "max_tokens": max(256, *expected/4+32)})
	start := time.Now()
	end := start.Add(*duration)
	var mu sync.Mutex
	all := make([]Sample, 0, 10000)
	var dropped atomic.Int64
	request := func(scheduled time.Time) {
		t := time.Now()
		s := Sample{StartMS: float64(t.Sub(start).Microseconds()) / 1000, QueueMS: float64(t.Sub(scheduled).Microseconds()) / 1000}
		req, _ := http.NewRequest("POST", *url, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+*key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			s.Error = err.Error()
		} else {
			s.Status = resp.StatusCode
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				s.Error = string(b)
			} else if *stream {
				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(make([]byte, 4096), 8<<20)
				done := false
				var last time.Time
				for scanner.Scan() {
					line := scanner.Bytes()
					s.Bytes += len(line) + 1
					if !bytes.HasPrefix(line, []byte("data:")) {
						continue
					}
					payload := bytes.TrimSpace(line[5:])
					if bytes.Equal(payload, []byte("[DONE]")) {
						done = true
						continue
					}
					var event struct {
						Choices []struct {
							Delta struct {
								Content string `json:"content"`
							} `json:"delta"`
						} `json:"choices"`
					}
					if err := json.Unmarshal(payload, &event); err != nil {
						s.Error = "invalid SSE JSON"
						continue
					}
					for _, choice := range event.Choices {
						if choice.Delta.Content != "" {
							now := time.Now()
							if s.Content == 0 {
								s.FirstContentMS = float64(now.Sub(t).Microseconds()) / 1000
							}
							if !last.IsZero() {
								s.MaxGapMS = max(s.MaxGapMS, float64(now.Sub(last).Microseconds())/1000)
							}
							last = now
							s.Content += len(choice.Delta.Content)
						}
					}
				}
				if scanner.Err() != nil {
					s.Error = scanner.Err().Error()
				}
				if !done && s.Error == "" {
					s.Error = "missing DONE"
				}
			} else {
				b, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
				s.Bytes = len(b)
				var event struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				if e != nil {
					s.Error = e.Error()
				} else if json.Unmarshal(b, &event) != nil {
					s.Error = "invalid JSON"
				} else {
					for _, c := range event.Choices {
						s.Content += len(c.Message.Content)
					}
				}
				s.FirstContentMS = float64(time.Since(t).Microseconds()) / 1000
			}
			resp.Body.Close()
			if s.Status == 200 && s.Content != *expected && s.Error == "" {
				s.Error = fmt.Sprintf("content bytes %d != %d", s.Content, *expected)
			}
		}
		s.LatencyMS = float64(time.Since(t).Microseconds())/1000 + s.QueueMS
		mu.Lock()
		all = append(all, s)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	if *rate == 0 {
		for i := 0; i < *concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(end) {
					request(time.Now())
				}
			}()
		}
		wg.Wait()
	} else {
		jobs := make(chan time.Time, *concurrency)
		for i := 0; i < *concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for t := range jobs {
					request(t)
				}
			}()
		}
		for i := int64(0); ; i++ {
			t := start.Add(time.Duration(float64(i) * float64(time.Second) / *rate))
			if !t.Before(end) {
				break
			}
			if d := time.Until(t); d > 0 {
				time.Sleep(d)
			}
			select {
			case jobs <- t:
			default:
				dropped.Add(1)
			}
		}
		close(jobs)
		wg.Wait()
	}
	elapsed := time.Since(start).Seconds()
	lat, first, gaps := []float64{}, []float64{}, []float64{}
	errors := map[string]int{}
	statuses := map[int]int{}
	good := 0
	wire := 0
	for _, s := range all {
		statuses[s.Status]++
		wire += s.Bytes
		if s.Error != "" {
			errors[s.Error]++
			continue
		}
		good++
		lat = append(lat, s.LatencyMS)
		first = append(first, s.FirstContentMS)
		gaps = append(gaps, s.MaxGapMS)
	}
	result := map[string]any{"requests": len(all), "success": good, "errors": errors, "statuses": statuses, "dropped_arrivals": dropped.Load(), "arrival_seconds": duration.Seconds(), "elapsed_seconds": elapsed, "success_rps": float64(good) / elapsed, "latency_ms": quant(lat), "first_content_ms": quant(first), "max_gap_ms": quant(gaps), "wire_bytes": wire, "concurrency": *concurrency, "rate": *rate, "stream": *stream, "prompt_bytes": *prompt, "expected_bytes": *expected, "samples": all}
	file, err := os.Create(*output)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	json.NewEncoder(file).Encode(result)
}
func main() {
	if len(os.Args) < 2 {
		panic("mock or load")
	}
	if os.Args[1] == "load" {
		load(os.Args[2:])
		return
	}
	if os.Args[1] != "mock" {
		panic("unknown mode")
	}
	f := flag.NewFlagSet("mock", flag.ExitOnError)
	addr := f.String("listen", ":8080", "listen address")
	f.Parse(os.Args[2:])
	m := &Mock{cfg: Scenario{Bytes: 1024, Chunks: 16}}
	server := &http.Server{Addr: *addr, Handler: m, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
