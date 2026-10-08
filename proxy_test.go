package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProxyPreservesBodyAndRemovesInternalCredentials(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":128000,"messages":[{"role":"user","content":"private text"}],"safeguards":{"enabled":true}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.URL.RawQuery != "beta=true" {
			t.Errorf("request changed: body=%s query=%s", got, r.URL.RawQuery)
		}
		if r.Header.Get("X-Local-Diag-Authorization") != "" || r.Header.Get("X-Local-Diag-X-Api-Key") != "" {
			t.Error("internal credential headers reached upstream")
		}
		if r.Header.Get("X-Api-Key") != "test-api-key" {
			t.Error("API credential was not forwarded")
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer upstream.Close()
	handler, err := newProxy(upstream.URL, BodyPolicy{}, nil, &snapshotStore{}, "")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(body))
	request.Header.Set("X-Api-Key", "test-api-key")
	request.Header.Set("X-Local-Diag-Authorization", "Bearer do-not-forward")
	request.Header.Set("X-Local-Diag-X-Api-Key", "do-not-forward")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("status %d", response.Code)
	}
}

func TestProxyStreamsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: last\n\n")
	}))
	defer upstream.Close()
	handler, err := newProxy(upstream.URL, BodyPolicy{}, nil, &snapshotStore{}, "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	first := make(chan string, 1)
	go func() {
		response, err := http.Get(server.URL + "/v1/messages")
		if err != nil {
			first <- "error"
			return
		}
		defer response.Body.Close()
		line, _ := bufio.NewReader(response.Body).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: first\n" {
			t.Errorf("first event %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Error("stream was buffered until upstream completed")
	}
	close(release)
}

func TestLegacyHeadersAreNormalizedWithoutChangingOtherBetas(t *testing.T) {
	for _, authentication := range []string{"api-key", "bearer"} {
		t.Run(authentication, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "sk-ant-usr-test-key" || r.Header.Get("Authorization") != "" {
					t.Error("legacy authentication was not normalized")
				}
				if r.Header.Get("Anthropic-Beta") != "other-feature-2026-01-01" || r.Header.Get("X-App") != "" {
					t.Error("OAuth flags were not removed independently")
				}
				if r.Header.Get("User-Agent") != "AnthropicBodyProxy/1.0" {
					t.Error("CLI identity remained")
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()
			handler, err := newProxy(upstream.URL, BodyPolicy{}, nil, &snapshotStore{}, "")
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","messages":[]}`))
			if authentication == "api-key" {
				request.Header.Set("X-Api-Key", "sk-ant-usr-test-key")
			} else {
				request.Header.Set("Authorization", "Bearer sk-ant-usr-test-key")
			}
			request.Header.Set("Anthropic-Beta", "claude-code-20250219,other-feature-2026-01-01,oauth-2025-04-20,claude-code-20250219")
			request.Header.Set("User-Agent", "claude-cli/2.1.294")
			request.Header.Set("X-App", "cli")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("status %d", response.Code)
			}
		})
	}
}

func TestOAuthCredentialsKeepRequiredIdentity(t *testing.T) {
	header := http.Header{}
	header.Set("Authorization", "Bearer sk-ant-oat01-test-token")
	header.Set("Anthropic-Beta", "oauth-2025-04-20")
	header.Set("User-Agent", "claude-cli/test")
	header.Set("X-App", "cli")
	if normalizeLegacyHeaders(header) {
		t.Fatal("OAuth credentials were treated as API keys")
	}
	if header.Get("Authorization") != "Bearer sk-ant-oat01-test-token" || header.Get("Anthropic-Beta") != "oauth-2025-04-20" || header.Get("X-App") != "cli" {
		t.Fatal("OAuth identity changed")
	}
}

func TestProxyBoilerplatePoliciesApplyOnlyToUserAPIKeys(t *testing.T) {
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: attribution"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Main instructions","cache_control":{"type":"ephemeral"}},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":"preserve"}],"tools":[{"name":"keep-tool"}],"thinking":{"type":"enabled","budget_tokens":1024},"metadata":{"user_id":"keep-user"},"max_tokens":128000,"safeguards":{"enabled":true}}`
	for _, test := range []struct {
		name, header, credential string
		strip                    bool
	}{
		{"user-api-key", "X-Api-Key", "sk-ant-usr-test-key", true},
		{"user-bearer", "Authorization", "Bearer sk-ant-usr-test-key", true},
		{"oauth", "Authorization", "Bearer sk-ant-oat01-test-token", false},
		{"ordinary-api-key", "X-Api-Key", "sk-ant-api03-test-key", false},
		{"no-credential", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarded := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				forwarded <- got
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			handler, err := newProxy(upstream.URL, BodyPolicy{StripClaudeAttribution: true, StripClaudeCodeIdentity: true}, nil, &snapshotStore{}, "")
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			if test.header != "" {
				request.Header.Set(test.header, test.credential)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("status %d", response.Code)
			}
			got := <-forwarded
			if !test.strip {
				if string(got) != body {
					t.Fatal("body identity changed for another credential type")
				}
				return
			}
			var before, after map[string]any
			if err := json.Unmarshal([]byte(body), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &after); err != nil {
				t.Fatal(err)
			}
			before["system"] = before["system"].([]any)[2:]
			if !reflect.DeepEqual(before, after) {
				t.Fatal("proxy changed fields beyond the two leading boilerplate blocks")
			}
		})
	}
}

func TestInspectorSnapshotOmitsAllCredentialHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer upstream.Close()
	store := &snapshotStore{}
	handler, err := newProxy(upstream.URL, BodyPolicy{}, nil, store, "")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"messages":[]}`))
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization", "Set-Cookie"} {
		request.Header.Set(name, "private-secret")
	}
	handler.ServeHTTP(httptest.NewRecorder(), request)
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization", "Set-Cookie"} {
		if store.get().Headers.Get(name) != "" {
			t.Errorf("snapshot exposed %s", name)
		}
	}
}
