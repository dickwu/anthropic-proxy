package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type requestState struct {
	ID             string
	Key            string
	OriginalKey    string
	OriginalBearer string
	Fields         map[string]any
	Debug          *debugRecord
}

type stateKey struct{}

var secretPattern = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)

func sanitize(text string, keys ...string) string {
	for _, key := range keys {
		if len(key) >= 8 {
			text = strings.ReplaceAll(text, key, "<REDACTED>")
		}
	}
	return secretPattern.ReplaceAllString(text, "<REDACTED>")
}

func logEvent(logger *slog.Logger, phase string, state *requestState, fields map[string]any) {
	if logger == nil {
		return
	}
	args := []any{"phase", phase, "request_id", state.ID}
	for key, value := range state.Fields {
		args = append(args, key, value)
	}
	for key, value := range fields {
		args = append(args, key, value)
	}
	logger.Info("proxy_request", args...)
}

func bodySummary(body []byte) map[string]any {
	fields := map[string]any{"request_bytes": len(body)}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		fields["valid_json"] = false
		return fields
	}
	names := make([]string, 0, len(payload))
	for name := range payload {
		names = append(names, name)
	}
	fields["body_fields"] = names
	for _, name := range []string{"model", "max_tokens", "stream"} {
		var value any
		if json.Unmarshal(payload[name], &value) == nil {
			if text, ok := value.(string); ok {
				value = sanitize(text)
			}
			fields[name] = value
		}
	}
	var system any
	json.Unmarshal(payload["system"], &system)
	attribution := false
	if text, ok := system.(string); ok {
		attribution = strings.HasPrefix(text, "x-anthropic-billing-header:")
	}
	if blocks, ok := system.([]any); ok && len(blocks) > 0 {
		if block, ok := blocks[0].(map[string]any); ok {
			text, _ := block["text"].(string)
			attribution = strings.HasPrefix(text, "x-anthropic-billing-header:")
		}
	}
	fields["claude_attribution_present"] = attribution
	_, fields["safeguards_present"] = payload["safeguards"]
	var tools []map[string]json.RawMessage
	json.Unmarshal(payload["tools"], &tools)
	types := []string{}
	for _, tool := range tools {
		var kind string
		if json.Unmarshal(tool["type"], &kind) == nil && kind != "" {
			types = append(types, sanitize(kind))
		}
	}
	fields["server_tool_types"] = types
	return fields
}

func newProxy(upstream string, policy BodyPolicy, logger *slog.Logger, snapshots *snapshotStore, expectedSHA string, debugDirs ...string) (http.Handler, error) {
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 900 * time.Second
	transport.DisableCompression = true
	debugDir := ""
	if len(debugDirs) > 0 {
		debugDir = debugDirs[0]
	}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport:     transport,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			req.Out.Host = target.Host
			for name := range req.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-local-diag-") {
					req.Out.Header.Del(name)
				}
			}
			req.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(response *http.Response) error {
			state, _ := response.Request.Context().Value(stateKey{}).(*requestState)
			if state == nil {
				return nil
			}
			fields := map[string]any{"status": response.StatusCode, "upstream_request_id": response.Header.Get("Request-Id")}
			var errorBody []byte
			if response.StatusCode >= 400 {
				prefix, err := io.ReadAll(io.LimitReader(response.Body, 65536))
				if err != nil {
					return err
				}
				response.Body = &combinedBody{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), closer: response.Body}
				errorBody = prefix
				var result struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal(prefix, &result) == nil {
					fields["error_type"] = result.Error.Type
					fields["error_message"] = sanitize(result.Error.Message, state.Key, state.OriginalKey, state.OriginalBearer)
				}
			}
			if err := state.Debug.response(response.StatusCode, response.Header, errorBody); err != nil {
				fields["debug_log_error"] = fmt.Sprintf("%T", err)
			}
			logEvent(logger, "upstream_headers", state, fields)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			state, _ := r.Context().Value(stateKey{}).(*requestState)
			if state != nil {
				logEvent(logger, "transport_error", state, map[string]any{"error_type": fmt.Sprintf("%T", err)})
			}
			http.Error(w, "upstream transport error", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			http.Error(w, "request body exceeds limit or could not be read", 413)
			return
		}
		originalHeaders := r.Header.Clone()
		originalBody := body
		id := r.Header.Get("X-Local-Diag-Request-Id")
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) {
			var value [16]byte
			if _, err := rand.Read(value[:]); err != nil {
				http.Error(w, "request ID generation failed", 500)
				return
			}
			id = hex.EncodeToString(value[:])
		}
		originalKey := r.Header.Get("X-Api-Key")
		originalBearer := bearerToken(r.Header.Get("Authorization"))
		normalized := normalizeLegacyHeaders(r.Header)
		state := &requestState{ID: id, Key: r.Header.Get("X-Api-Key"), OriginalKey: originalKey, OriginalBearer: originalBearer, Fields: bodySummary(body)}
		state.Fields["legacy_headers_normalized"] = normalized
		state.Fields["uri"] = r.URL.Path
		if expectedSHA != "" {
			digest := sha256.Sum256([]byte(state.Key))
			state.Fields["selected_key_matches_tested"] = hex.EncodeToString(digest[:]) == expectedSHA
		}
		snapshotHeaders := r.Header.Clone()
		for name := range snapshotHeaders {
			_, credential := debugCredentialNames[strings.ToLower(name)]
			if strings.HasPrefix(strings.ToLower(name), "x-local-diag-") || credential {
				snapshotHeaders.Del(name)
			}
		}
		if r.Method == "POST" && r.URL.Path == "/v1/messages" {
			snapshots.set(requestSnapshot{Method: r.Method, URI: r.URL.RequestURI(), Headers: snapshotHeaders, Body: body})
		}
		changed := []string{}
		if r.URL.Path == "/v1/messages" {
			body, changed, err = transformBody(body, policy)
			if err != nil {
				http.Error(w, "configured body rewrite could not be applied", 400)
				return
			}
		}
		state.Fields["body_changes"] = changed
		state.Debug, err = newDebugRecord(debugDir, id, r.Method, r.URL.RequestURI(), originalHeaders, originalBody, body, changed, []string{state.Key, originalKey, originalBearer})
		if err != nil {
			state.Fields["debug_log_error"] = fmt.Sprintf("%T", err)
		}
		logEvent(logger, "request_received", state, nil)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Del("Content-Length")
		r.TransferEncoding = nil
		r = r.WithContext(context.WithValue(r.Context(), stateKey{}, state))
		started := time.Now()
		proxy.ServeHTTP(w, r)
		logEvent(logger, "completed", state, map[string]any{"seconds": time.Since(started).Seconds()})
	}), nil
}

func bearerToken(value string) string {
	parts := strings.Fields(value)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func normalizeLegacyHeaders(header http.Header) bool {
	key := header.Get("X-Api-Key")
	if key == "" {
		key = bearerToken(header.Get("Authorization"))
	}
	if !strings.HasPrefix(key, "sk-ant-usr-") {
		return false
	}
	header.Set("X-Api-Key", key)
	header.Del("Authorization")
	flags := []string{}
	for _, value := range header.Values("Anthropic-Beta") {
		for _, flag := range strings.Split(value, ",") {
			flag = strings.TrimSpace(flag)
			if flag != "" && flag != "claude-code-20250219" && flag != "oauth-2025-04-20" {
				flags = append(flags, flag)
			}
		}
	}
	header.Del("Anthropic-Beta")
	if len(flags) > 0 {
		header.Set("Anthropic-Beta", strings.Join(flags, ","))
	}
	header.Del("X-App")
	if agent := header.Get("User-Agent"); strings.Contains(agent, "claude-cli/") || strings.Contains(agent, "claude-code/") {
		header.Set("User-Agent", "AnthropicBodyProxy/1.0")
	}
	return true
}

type combinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *combinedBody) Close() error { return b.closer.Close() }
