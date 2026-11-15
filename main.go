// dokploy-opencode-go-proxy — OpenAI-compatible passthrough that makes
// Dokploy's AI panel (Settings → AI → Add AI Provider) work against the
// OpenCode Go gateway.
//
// Why it exists: Dokploy builds its provider with @ai-sdk/openai-compatible and
// hardcodes the Authorization header only (packages/server/src/utils/ai/select-ai-provider.ts).
// OpenCode Go rejects every non-GET request without a stable `x-opencode-session`
// header, so the panel cannot talk to https://opencode.ai/inference/go directly.
//
// What it does: forwards /v1/models and /v1/chat/completions upstream, adding
// `x-opencode-session` (derived deterministically per Dokploy feature) and its
// own User-Agent, and streams the response back untouched.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	listen      string
	upstream    string // full base URL, e.g. https://opencode.ai/inference/go/openai/v1
	apiKey      string // if set, injected upstream; otherwise the client's Authorization is forwarded
	proxyToken  string // if set, the client must send "Authorization: Bearer <proxyToken>"
	userAgent   string
	sessionMode string // prefix | constant | request
	sessionID   string // used by SESSION_MODE=constant
	prefixLen   int
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	cfg := config{
		listen:      env("LISTEN_ADDR", ":8080"),
		upstream:    strings.TrimRight(env("UPSTREAM_BASE", "https://opencode.ai/inference/go/openai/v1"), "/"),
		apiKey:      env("OPENCODE_API_KEY", ""),
		proxyToken:  env("PROXY_TOKEN", ""),
		userAgent:   env("USER_AGENT", "dokploy-ai/1.0"),
		sessionMode: strings.ToLower(env("SESSION_MODE", "prefix")),
		sessionID:   env("SESSION_ID", "ses_dokploy_proxy"),
		prefixLen:   256,
	}
	if cfg.sessionMode != "prefix" && cfg.sessionMode != "constant" && cfg.sessionMode != "request" {
		log.Fatalf("SESSION_MODE must be prefix, constant or request (got %q)", cfg.sessionMode)
	}
	return cfg
}

// --- session derivation ---------------------------------------------------

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages []chatMessage   `json:"messages"`
	Prompt   json.RawMessage `json:"prompt"`
}

// contentText accepts both plain strings and the multimodal parts array.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func firstText(req chatRequest) string {
	for _, m := range req.Messages {
		if t := contentText(m.Content); t != "" {
			return t
		}
	}
	return contentText(req.Prompt)
}

// sessionFor derives a session id that is stable for one Dokploy feature and
// different between features. Dokploy sends a single prompt per request with no
// conversation history, so "one conversation" has to be approximated: the
// instruction prefix of the prompt is identical for every call of the same
// feature (log analysis, compose generation, variant suggestion) and differs
// between them, which is exactly the affinity Go's routing wants.
func (c config) sessionFor(body []byte) string {
	switch c.sessionMode {
	case "constant":
		return c.sessionID
	case "request":
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return c.sessionID
		}
		return "ses_" + hex.EncodeToString(buf)
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return c.sessionID
	}
	text := firstText(req)
	if len(text) > c.prefixLen {
		text = text[:c.prefixLen]
	}
	h := sha256.New()
	io.WriteString(h, req.Model)
	h.Write([]byte{0})
	io.WriteString(h, text)
	return "ses_" + hex.EncodeToString(h.Sum(nil))[:24]
}

// --- forwarding -----------------------------------------------------------

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (c config) upstreamURL(r *http.Request) string {
	p := r.URL.Path
	switch {
	case p == "/v1":
		p = ""
	case strings.HasPrefix(p, "/v1/"):
		p = p[3:]
	}
	u := c.upstream + p
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return u
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "proxy_error"},
	})
}

func (c config) ServeHTTP(client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "ok\n")
			return
		}

		auth := r.Header.Get("Authorization")
		if c.proxyToken != "" && auth != "Bearer "+c.proxyToken {
			writeJSONError(w, http.StatusUnauthorized, "proxy: bad or missing Authorization token")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "proxy: cannot read request body: "+err.Error())
			return
		}

		session := ""
		if r.Method != http.MethodGet {
			session = c.sessionFor(body)
		}

		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, c.upstreamURL(r), bytes.NewReader(body))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "proxy: cannot build upstream request: "+err.Error())
			return
		}
		copyHeader(upstreamReq.Header, r.Header)
		for _, h := range []string{"Authorization", "X-Opencode-Session", "User-Agent", "Content-Length", "Host", "Accept-Encoding"} {
			upstreamReq.Header.Del(h)
		}

		switch {
		case c.apiKey != "":
			upstreamReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		case auth != "":
			upstreamReq.Header.Set("Authorization", auth)
		}
		upstreamReq.Header.Set("User-Agent", c.userAgent)
		if session != "" {
			upstreamReq.Header.Set("x-opencode-session", session)
			w.Header().Set("X-Proxy-Session", session)
		}
		if len(body) > 0 {
			upstreamReq.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}

		start := time.Now()
		resp, err := client.Do(upstreamReq)
		if err != nil {
			log.Printf("%s %s upstream_error=%v", r.Method, r.URL.Path, err)
			writeJSONError(w, http.StatusBadGateway, "proxy: upstream request failed: "+err.Error())
			return
		}
		defer resp.Body.Close()

		log.Printf("%s %s -> %d in %s session=%q", r.Method, r.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond), session)

		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		flusher, canFlush := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if canFlush {
					flusher.Flush()
				}
			}
			if rerr != nil {
				return
			}
		}
	}
}

func main() {
	cfg := loadConfig()

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true, // keep bodies byte-exact; we never ask for gzip
	}
	client := &http.Client{Transport: transport}

	keySource := "client Authorization"
	if cfg.apiKey != "" {
		keySource = "OPENCODE_API_KEY"
	}
	log.Printf("listening on %s -> %s (session_mode=%s, key=%s, proxy_token=%v)",
		cfg.listen, cfg.upstream, cfg.sessionMode, keySource, cfg.proxyToken != "")

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           cfg.ServeHTTP(client),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
