package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const anthropicBillingHeaderPrefix = "x-anthropic-billing-header:"
const claudeCodeIdentityText = "You are Claude Code, Anthropic's official CLI for Claude."
const claudeAgentSDKIdentityText = "You are a Claude agent, built on Anthropic's Claude Agent SDK."

type BodyPolicy struct {
	StripClaudeAttribution  bool `json:"strip_claude_attribution"`
	StripClaudeCodeIdentity bool `json:"strip_claude_code_identity"`
	DropMetadataUserID      bool `json:"drop_metadata_user_id"`
	MaxOutputTokens         int  `json:"max_output_tokens"`
}

func transformBody(data []byte, policy BodyPolicy) ([]byte, []string, error) {
	if !policy.StripClaudeAttribution && !policy.StripClaudeCodeIdentity && !policy.DropMetadataUserID && policy.MaxOutputTokens <= 0 {
		return data, nil, nil
	}

	root, err := decodeObject(data)
	if err != nil {
		return nil, nil, err
	}

	var actions []string
	if policy.StripClaudeAttribution {
		changed, err := stripSystemAttribution(root)
		if err != nil {
			return nil, nil, err
		}
		if changed {
			actions = append(actions, "stripped_system_attribution")
		}
	}
	if policy.StripClaudeCodeIdentity {
		changed, err := stripClaudeCodeIdentity(root)
		if err != nil {
			return nil, nil, err
		}
		if changed {
			actions = append(actions, "stripped_claude_code_identity")
		}
	}
	if policy.DropMetadataUserID {
		changed, err := dropMetadataUserID(root)
		if err != nil {
			return nil, nil, err
		}
		if changed {
			actions = append(actions, "dropped_metadata_user_id")
		}
	}
	if policy.MaxOutputTokens > 0 {
		changed, err := capMaxTokens(root, policy.MaxOutputTokens)
		if err != nil {
			return nil, nil, err
		}
		if changed {
			actions = append(actions, "capped_max_tokens")
		}
	}

	if len(actions) == 0 {
		return data, nil, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, nil, err
	}
	return out, actions, nil
}

func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var root map[string]json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("decode body: expected JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("decode body: trailing JSON content")
	}
	return root, nil
}

func stripSystemAttribution(root map[string]json.RawMessage) (bool, error) {
	system, ok := root["system"]
	if !ok {
		return false, nil
	}

	var scalar string
	if err := json.Unmarshal(system, &scalar); err == nil {
		rewritten, changed := stripFirstAttributionLine(scalar)
		if !changed {
			return false, nil
		}
		encoded, err := json.Marshal(rewritten)
		if err != nil {
			return false, err
		}
		root["system"] = encoded
		return true, nil
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(system, &blocks); err != nil {
		return false, nil
	}
	if len(blocks) == 0 {
		return false, nil
	}

	text, ok, err := textFromBlock(blocks[0])
	if err != nil {
		return false, err
	}
	if ok && strings.HasPrefix(text, anthropicBillingHeaderPrefix) {
		encoded, err := json.Marshal(blocks[1:])
		if err != nil {
			return false, err
		}
		root["system"] = encoded
		return true, nil
	}
	return false, nil
}

func stripClaudeCodeIdentity(root map[string]json.RawMessage) (bool, error) {
	system, ok := root["system"]
	if !ok {
		return false, nil
	}

	var scalar string
	if err := json.Unmarshal(system, &scalar); err == nil {
		rewritten, changed := stripClaudeCodeIdentityLine(scalar)
		if !changed {
			return false, nil
		}
		encoded, err := json.Marshal(rewritten)
		if err != nil {
			return false, err
		}
		root["system"] = encoded
		return true, nil
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(system, &blocks); err != nil {
		return false, nil
	}
	index := claudeCodeIdentityBlockIndex(blocks)
	if index < 0 {
		return false, nil
	}
	blocks = append(blocks[:index], blocks[index+1:]...)
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return false, err
	}
	root["system"] = encoded
	return true, nil
}

func claudeCodeIdentityBlockIndex(blocks []json.RawMessage) int {
	if len(blocks) == 0 {
		return -1
	}
	if blockIsClaudeIdentity(blocks[0]) {
		return 0
	}
	if len(blocks) > 1 && blockTextHasPrefix(blocks[0], anthropicBillingHeaderPrefix) && blockIsClaudeIdentity(blocks[1]) {
		return 1
	}
	return -1
}

func stripFirstAttributionLine(system string) (string, bool) {
	lines := strings.SplitAfter(system, "\n")
	if len(lines) == 0 {
		return system, false
	}

	trimmed := strings.TrimSuffix(lines[0], "\n")
	if strings.HasPrefix(trimmed, anthropicBillingHeaderPrefix) {
		return strings.Join(lines[1:], ""), true
	}
	return system, false
}

func stripClaudeCodeIdentityLine(system string) (string, bool) {
	lines := strings.SplitAfter(system, "\n")
	if len(lines) == 0 {
		return system, false
	}
	if isClaudeIdentityText(lineText(lines[0])) {
		return strings.Join(lines[1:], ""), true
	}
	if len(lines) > 1 && strings.HasPrefix(lineText(lines[0]), anthropicBillingHeaderPrefix) && isClaudeIdentityText(lineText(lines[1])) {
		return strings.Join(append(lines[:1], lines[2:]...), ""), true
	}
	return system, false
}

func lineText(line string) string {
	return strings.TrimSuffix(line, "\n")
}

func isClaudeIdentityText(text string) bool {
	return text == claudeCodeIdentityText || text == claudeAgentSDKIdentityText
}

func blockIsClaudeIdentity(block json.RawMessage) bool {
	text, ok, err := textFromBlock(block)
	return err == nil && ok && isClaudeIdentityText(text)
}

func blockTextHasPrefix(block json.RawMessage, prefix string) bool {
	text, ok, err := textFromBlock(block)
	return err == nil && ok && strings.HasPrefix(text, prefix)
}

func textFromBlock(block json.RawMessage) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(block, &fields); err != nil {
		return "", false, nil
	}

	rawType, ok := fields["type"]
	if !ok {
		return "", false, nil
	}
	var blockType string
	if err := json.Unmarshal(rawType, &blockType); err != nil || blockType != "text" {
		return "", false, nil
	}

	rawText, ok := fields["text"]
	if !ok {
		return "", false, nil
	}
	var text string
	if err := json.Unmarshal(rawText, &text); err != nil {
		return "", false, nil
	}
	return text, true, nil
}

func dropMetadataUserID(root map[string]json.RawMessage) (bool, error) {
	metadata, ok := root["metadata"]
	if !ok {
		return false, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return false, nil
	}
	if _, ok := fields["user_id"]; !ok {
		return false, nil
	}
	delete(fields, "user_id")

	encoded, err := json.Marshal(fields)
	if err != nil {
		return false, err
	}
	root["metadata"] = encoded
	return true, nil
}

func capMaxTokens(root map[string]json.RawMessage, cap int) (bool, error) {
	rawMaxTokens, ok := root["max_tokens"]
	if !ok {
		return false, nil
	}

	current, ok := parsePositiveInt(rawMaxTokens)
	if !ok || current <= cap {
		return false, nil
	}

	if budget, ok := explicitThinkingBudget(root); ok && cap <= budget {
		return false, fmt.Errorf("max_tokens cap %d would be <= explicit thinking.budget_tokens %d", cap, budget)
	}

	root["max_tokens"] = json.RawMessage(strconv.Itoa(cap))
	return true, nil
}

func explicitThinkingBudget(root map[string]json.RawMessage) (int, bool) {
	thinking, ok := root["thinking"]
	if !ok {
		return 0, false
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(thinking, &fields); err != nil {
		return 0, false
	}
	rawBudget, ok := fields["budget_tokens"]
	if !ok {
		return 0, false
	}
	return parsePositiveInt(rawBudget)
}

func parsePositiveInt(raw json.RawMessage) (int, bool) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, false
	}
	value, err := strconv.Atoi(number.String())
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}
