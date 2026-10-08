package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewDebugRecordWritesSanitizedDetailedRequestAndResponse(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "debug")
	before := []byte(`{"model":"claude-opus-5-5","system":"keep exact prompt detail","messages":[{"role":"user","content":"diagnose this real prompt"}],"api_key":"sk-ant-secret","metadata":{"trace_id":"trace-1","token":"secret-token","note":"keep note"},"env":{"ANTHROPIC_API_KEY":"env-secret","safe":"keep env safe"},"secret_string":"manual-secret"}`)
	after := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"diagnose this real prompt"}],"max_tokens":4096}`)
	headers := http.Header{
		"Authorization":              {"Bearer auth-secret"},
		"X-Api-Key":                  {"sk-ant-header"},
		"Cookie":                     {"session=secret"},
		"X-Local-Diag-Authorization": {"Bearer local-auth"},
		"X-Local-Diag-X-Api-Key":     {"local-key"},
		"Anthropic-Beta":             {"fine-grained-tool-streaming-2025-05-14"},
		"X-Request-Name":             {"debug prompt detail"},
	}

	record, err := newDebugRecord(directory, "req-1", "POST", "/v1/messages?api_key=sk-ant-query&prompt=keep+query+detail&token=query-secret", headers, before, after, []string{"stripped_system_attribution", "capped_max_tokens"}, []string{"manual-secret"})
	if err != nil {
		t.Fatalf("newDebugRecord returned error: %v", err)
	}
	responseHeaders := http.Header{
		"Set-Cookie":   {"session=response-secret"},
		"Request-Id":   {"req-upstream"},
		"Content-Type": {"application/json"},
	}
	if err := record.response(429, responseHeaders, []byte(`{"error":{"message":"bad sk-ant-error manual-secret","password":"response-password"},"detail":"keep error detail"}`)); err != nil {
		t.Fatalf("response returned error: %v", err)
	}

	var got map[string]any
	readDebugJSON(t, filepath.Join(directory, "req-1.json"), &got)
	encoded := mustJSON(t, got)

	for _, secret := range []string{"auth-secret", "sk-ant-header", "session=secret", "local-auth", "local-key", "sk-ant-query", "secret-token", "env-secret", "manual-secret", "response-secret", "response-password", "sk-ant-error", "query-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("debug record leaked %q in %s", secret, encoded)
		}
	}
	for _, detail := range []string{"keep exact prompt detail", "diagnose this real prompt", "keep note", "keep env safe", "fine-grained-tool-streaming-2025-05-14", "stripped_system_attribution", "capped_max_tokens", "keep error detail", "req-upstream", "4096"} {
		if !strings.Contains(encoded, detail) {
			t.Fatalf("debug record lost legitimate detail %q in %s", detail, encoded)
		}
	}
	uri, ok := got["uri"].(string)
	if !ok {
		t.Fatalf("uri missing or wrong shape: %#v", got["uri"])
	}
	if !strings.Contains(uri, "prompt=keep+query+detail") {
		t.Fatalf("debug record lost query detail in uri %q", uri)
	}
	if got["method"] != "POST" {
		t.Fatalf("method = %v, want POST", got["method"])
	}
	response, ok := got["response"].(map[string]any)
	if !ok {
		t.Fatalf("response missing or wrong shape: %#v", got["response"])
	}
	if response["status"] != float64(429) {
		t.Fatalf("status = %v, want 429", response["status"])
	}
}

func TestDebugRecordPrivateModesAndUnsafeExistingDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "private-debug")

	record, err := newDebugRecord(directory, "req-private", "GET", "/v1/messages", nil, []byte(`{"prompt":"keep"}`), nil, nil, nil)
	if err != nil {
		t.Fatalf("newDebugRecord returned error: %v", err)
	}
	if record == nil {
		t.Fatal("record is nil for non-empty directory")
	}
	assertMode(t, directory, 0o700)
	assertMode(t, filepath.Join(directory, "req-private.json"), 0o600)

	unsafeDir := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = newDebugRecord(unsafeDir, "req-unsafe", "GET", "/v1/messages", nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected unsafe existing directory to be rejected")
	}
	assertMode(t, unsafeDir, 0o755)
}

func TestDebugRecordIgnoresEmptySecretsWithoutCorruptingReadableDetail(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "debug")
	secrets := []string{"known-secret", "", ""}
	record, err := newDebugRecord(
		directory,
		"req-empty-secret",
		"POST",
		"/v1/messages?prompt=ordinary+query+detail&trace=known-secret",
		http.Header{"X-Debug-Name": {"ordinary header detail known-secret"}},
		[]byte(`{"system":"ordinary prompt detail known-secret","messages":[{"role":"user","content":"ordinary user detail"}]}`),
		nil,
		nil,
		secrets,
	)
	if err != nil {
		t.Fatalf("newDebugRecord returned error: %v", err)
	}
	if err := record.response(400, http.Header{"X-Debug-Response": {"ordinary response detail known-secret"}}, []byte(`{"detail":"ordinary error detail known-secret"}`)); err != nil {
		t.Fatalf("response returned error: %v", err)
	}

	var got map[string]any
	readDebugJSON(t, filepath.Join(directory, "req-empty-secret.json"), &got)
	encoded := mustJSON(t, got)
	if strings.Contains(encoded, "known-secret") {
		t.Fatalf("known secret was not redacted: %s", encoded)
	}
	for _, detail := range []string{"ordinary header detail", "ordinary prompt detail", "ordinary user detail", "ordinary response detail", "ordinary error detail"} {
		if !strings.Contains(encoded, detail) {
			t.Fatalf("ordinary detail %q was corrupted: %s", detail, encoded)
		}
	}
	uri, ok := got["uri"].(string)
	if !ok {
		t.Fatalf("uri missing or wrong shape: %#v", got["uri"])
	}
	parsedURI, err := url.ParseRequestURI(uri)
	if err != nil {
		t.Fatalf("uri is not parseable: %v", err)
	}
	if parsedURI.Query().Get("prompt") != "ordinary query detail" {
		t.Fatalf("ordinary query detail was corrupted in uri %q", uri)
	}
}

func TestDebugRecordValidatesIDAndNilDirectoryAndNilReceiver(t *testing.T) {
	record, err := newDebugRecord("", "ignored", "GET", "/v1/messages", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("empty directory returned error: %v", err)
	}
	if record != nil {
		t.Fatalf("record = %#v, want nil", record)
	}
	if err := (*debugRecord)(nil).response(200, nil, nil); err != nil {
		t.Fatalf("nil response returned error: %v", err)
	}

	_, err = newDebugRecord(t.TempDir(), "../escape", "GET", "/v1/messages", nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected traversal id to be rejected")
	}
}

func TestDebugRecordResponseIsSafeForConcurrentCallers(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "debug")
	record, err := newDebugRecord(directory, "req-concurrent", "POST", "/v1/messages", nil, []byte(`{"prompt":"keep detail"}`), []byte(`{"prompt":"keep after"}`), []string{"changed"}, nil)
	if err != nil {
		t.Fatalf("newDebugRecord returned error: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(status int) {
			defer wg.Done()
			if err := record.response(status, http.Header{"X-Api-Key": {"sk-ant-concurrent"}}, []byte(`{"detail":"keep concurrent detail"}`)); err != nil {
				t.Errorf("response returned error: %v", err)
			}
		}(500 + i)
	}
	wg.Wait()

	var got map[string]any
	readDebugJSON(t, filepath.Join(directory, "req-concurrent.json"), &got)
	encoded := mustJSON(t, got)
	if strings.Contains(encoded, "sk-ant-concurrent") {
		t.Fatalf("concurrent response leaked credential: %s", encoded)
	}
	if !strings.Contains(encoded, "keep concurrent detail") {
		t.Fatalf("concurrent response lost error detail: %s", encoded)
	}
}

func readDebugJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("invalid JSON in %s: %v\n%s", path, err, data)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o, want %#o", path, got, want)
	}
}
