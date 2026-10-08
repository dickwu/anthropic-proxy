package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTransformBodyNoPolicyPreservesExactBytes(t *testing.T) {
	input := []byte("{\n  \"max_tokens\": 1000,\n  \"metadata\": {\"user_id\":\"u-1\"},\n  \"system\": \"x-anthropic-billing-header: keep when disabled\\nReal instruction\"\n}")

	got, actions, err := transformBody(input, BodyPolicy{})
	if err != nil {
		t.Fatalf("transformBody returned error: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("body changed without policy:\nwant %q\n got %q", input, got)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
}

func TestTransformBodyStripsOnlyFirstSystemAttributionBlock(t *testing.T) {
	input := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: internal"},{"type":"text","text":"system instruction"},{"type":"thinking","thinking":"preserve","signature":"sig"},{"type":"text","text":"x-anthropic-billing-header: user-visible"}],"messages":[{"role":"user","content":[{"type":"text","text":"x-anthropic-billing-header: preserve user text"}]}]}`)

	got, actions, err := transformBody(input, BodyPolicy{StripClaudeAttribution: true})
	if err != nil {
		t.Fatalf("transformBody returned error: %v", err)
	}
	if !hasAction(actions, "stripped_system_attribution") {
		t.Fatalf("actions = %v, want stripped_system_attribution", actions)
	}

	var decoded struct {
		System   []json.RawMessage `json:"system"`
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, got)
	}
	if len(decoded.System) != 3 {
		t.Fatalf("system block count = %d, want 3: %s", len(decoded.System), got)
	}
	if strings.Contains(string(decoded.System[0]), "x-anthropic-billing-header: internal") {
		t.Fatalf("first attribution block was not removed: %s", got)
	}
	if !strings.Contains(string(got), `"thinking":"preserve"`) || !strings.Contains(string(got), `"signature":"sig"`) {
		t.Fatalf("thinking/signature content was not preserved: %s", got)
	}
	if !strings.Contains(string(got), "x-anthropic-billing-header: user-visible") {
		t.Fatalf("later/user-visible attribution text was not preserved: %s", got)
	}
	if decoded.Messages[0].Content[0].Text != "x-anthropic-billing-header: preserve user text" {
		t.Fatalf("user content changed: %s", got)
	}
}

func TestTransformBodyStripsOnlyAttributionLineFromSystemString(t *testing.T) {
	input := []byte(`{"system":"x-anthropic-billing-header: internal\nKeep this instruction\nKeep this too","messages":[{"role":"user","content":"hello"}]}`)

	got, _, err := transformBody(input, BodyPolicy{StripClaudeAttribution: true})
	if err != nil {
		t.Fatalf("transformBody returned error: %v", err)
	}
	if strings.Contains(string(got), "x-anthropic-billing-header") {
		t.Fatalf("attribution line was not removed: %s", got)
	}
	if !strings.Contains(string(got), "Keep this instruction\\nKeep this too") {
		t.Fatalf("system instructions were not preserved: %s", got)
	}
	if !strings.Contains(string(got), `"messages":[`) {
		t.Fatalf("non-system content was not preserved: %s", got)
	}
}

func TestTransformBodyPreservesLaterAttributionLineInSystemString(t *testing.T) {
	input := []byte(`{"system":"Keep this instruction\nx-anthropic-billing-header: user instruction\nKeep this too","messages":[{"role":"user","content":"hello"}]}`)

	got, actions, err := transformBody(input, BodyPolicy{StripClaudeAttribution: true})
	if err != nil {
		t.Fatalf("transformBody returned error: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("later attribution line changed:\nwant %s\n got %s", input, got)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
}

func TestTransformBodyPreservesLaterAttributionBlockWhenFirstBlockIsNotText(t *testing.T) {
	input := []byte(`{"system":[{"type":"thinking","thinking":"preserve","signature":"sig"},{"type":"text","text":"x-anthropic-billing-header: user instruction"},{"type":"text","text":"keep"}],"messages":[{"role":"user","content":"hello"}]}`)

	got, actions, err := transformBody(input, BodyPolicy{StripClaudeAttribution: true})
	if err != nil {
		t.Fatalf("transformBody returned error: %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("later attribution block changed:\nwant %s\n got %s", input, got)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
}

func TestTransformBodyDropsOnlyMetadataUserID(t *testing.T) {
	input := []byte(`{"metadata":{"user_id":"u-1","trace_id":"t-1","nested":{"user_id":"keep-nested"}},"max_tokens":1000}`)

	got, actions, err := transformBody(input, BodyPolicy{DropMetadataUserID: true})
	if err != nil {
		t.Fatalf("transformBody returned error: %v", err)
	}
	if !hasAction(actions, "dropped_metadata_user_id") {
		t.Fatalf("actions = %v, want dropped_metadata_user_id", actions)
	}
	if strings.Contains(string(got), `"user_id":"u-1"`) {
		t.Fatalf("metadata.user_id was not removed: %s", got)
	}
	if !strings.Contains(string(got), `"trace_id":"t-1"`) || !strings.Contains(string(got), `"nested":{"user_id":"keep-nested"}`) {
		t.Fatalf("other metadata was not preserved: %s", got)
	}
}

func TestTransformBodyCapsMaxTokensUnlessThinkingBudgetWouldBecomeInvalid(t *testing.T) {
	input := []byte(`{"max_tokens":1000,"thinking":{"type":"enabled","budget_tokens":200}}`)

	got, actions, err := transformBody(input, BodyPolicy{MaxOutputTokens: 300})
	if err != nil {
		t.Fatalf("transformBody returned error: %v", err)
	}
	if !hasAction(actions, "capped_max_tokens") {
		t.Fatalf("actions = %v, want capped_max_tokens", actions)
	}
	if !strings.Contains(string(got), `"max_tokens":300`) {
		t.Fatalf("max_tokens was not capped: %s", got)
	}

	_, _, err = transformBody(input, BodyPolicy{MaxOutputTokens: 200})
	if err == nil {
		t.Fatal("expected error when cap is <= explicit thinking.budget_tokens")
	}
	if !strings.Contains(err.Error(), "thinking.budget_tokens") {
		t.Fatalf("error = %v, want thinking budget context", err)
	}
}

func hasAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}
