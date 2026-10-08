package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const redactedValue = "<REDACTED>"

var (
	debugIDPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	debugSecretPattern   = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)
	debugCredentialNames = map[string]struct{}{
		"authorization":              {},
		"cookie":                     {},
		"proxy-authorization":        {},
		"set-cookie":                 {},
		"x-api-key":                  {},
		"x-local-diag-authorization": {},
		"x-local-diag-x-api-key":     {},
	}
	debugSensitiveJSONKeys = map[string]struct{}{
		"access_token":  {},
		"api_key":       {},
		"password":      {},
		"refresh_token": {},
		"secret":        {},
		"token":         {},
	}
)

type debugRecord struct {
	mu      sync.Mutex
	path    string
	secrets []string
	data    map[string]any
}

func newDebugRecord(directory, id string, method, uri string, headers http.Header, before, after []byte, changes []string, secrets []string) (*debugRecord, error) {
	if directory == "" {
		return nil, nil
	}
	if err := validateDebugID(id); err != nil {
		return nil, err
	}
	if err := ensurePrivateDebugDirectory(directory); err != nil {
		return nil, err
	}
	safeSecrets := compactSecrets(secrets)

	record := &debugRecord{
		path:    filepath.Join(directory, id+".json"),
		secrets: safeSecrets,
		data: map[string]any{
			"id":      id,
			"method":  method,
			"uri":     sanitizeDebugURI(uri, safeSecrets),
			"headers": sanitizeDebugHeaders(headers, safeSecrets),
			"before":  sanitizeDebugBody(before, safeSecrets),
			"after":   sanitizeDebugBody(after, safeSecrets),
			"changes": append([]string(nil), changes...),
		},
	}
	if err := record.writeLocked(); err != nil {
		return nil, err
	}
	return record, nil
}

func (r *debugRecord) response(status int, headers http.Header, errorBody []byte) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	response := map[string]any{
		"status":  status,
		"headers": sanitizeDebugHeaders(headers, r.secrets),
	}
	if len(errorBody) > 0 {
		response["error_body"] = sanitizeDebugBody(errorBody, r.secrets)
	}
	r.data["response"] = response
	return r.writeLocked()
}

func validateDebugID(id string) error {
	if id == "" || id == "." || id == ".." || !debugIDPattern.MatchString(id) {
		return fmt.Errorf("invalid debug id")
	}
	if filepath.Base(id) != id {
		return fmt.Errorf("invalid debug id")
	}
	return nil
}

func ensurePrivateDebugDirectory(directory string) error {
	info, err := os.Stat(directory)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("debug path is not a directory")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("debug directory must be private")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(directory, 0o700)
}

func (r *debugRecord) writeLocked() error {
	data, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(filepath.Dir(r.path), ".debug-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(tempName)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, r.path); err != nil {
		return err
	}
	done = true
	return nil
}

func sanitizeDebugURI(uri string, secrets []string) string {
	parsed, err := url.ParseRequestURI(uri)
	if err != nil {
		return sanitizeDebugString(uri, secrets)
	}
	query := parsed.Query()
	for key, values := range query {
		if isSensitiveJSONKey(key) {
			for i := range values {
				values[i] = redactedValue
			}
			query[key] = values
			continue
		}
		for i, value := range values {
			values[i] = sanitizeDebugString(value, secrets)
		}
		query[key] = values
	}
	parsed.RawQuery = query.Encode()
	return sanitizeDebugString(parsed.String(), secrets)
}

func sanitizeDebugHeaders(headers http.Header, secrets []string) map[string][]string {
	if headers == nil {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(headers))
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values := append([]string(nil), headers[name]...)
		if _, sensitive := debugCredentialNames[strings.ToLower(name)]; sensitive {
			for i := range values {
				values[i] = redactedValue
			}
		} else {
			for i, value := range values {
				values[i] = sanitizeDebugString(value, secrets)
			}
		}
		out[name] = values
	}
	return out
}

func sanitizeDebugBody(body []byte, secrets []string) any {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return sanitizeDebugString(string(body), secrets)
	}
	return sanitizeDebugValue(value, secrets)
}

func sanitizeDebugValue(value any, secrets []string) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if isSensitiveJSONKey(key) {
				out[key] = redactedValue
				continue
			}
			out[key] = sanitizeDebugValue(child, secrets)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = sanitizeDebugValue(child, secrets)
		}
		return out
	case string:
		return sanitizeDebugString(typed, secrets)
	default:
		return value
	}
}

func isSensitiveJSONKey(key string) bool {
	normalized := strings.ToLower(key)
	if _, ok := debugSensitiveJSONKeys[normalized]; ok {
		return true
	}
	envName := strings.ToUpper(strings.ReplaceAll(normalized, "-", "_"))
	return hasCredentialEnvPart(envName, "PASSWORD") ||
		hasCredentialEnvPart(envName, "SECRET") ||
		hasCredentialEnvPart(envName, "TOKEN") ||
		strings.HasSuffix(envName, "API_KEY") ||
		strings.HasSuffix(envName, "ACCESS_KEY_ID")
}

func hasCredentialEnvPart(name, part string) bool {
	return name == part ||
		strings.HasPrefix(name, part+"_") ||
		strings.HasSuffix(name, "_"+part) ||
		strings.Contains(name, "_"+part+"_")
}

func sanitizeDebugString(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, redactedValue)
	}
	return debugSecretPattern.ReplaceAllString(text, redactedValue)
}

func compactSecrets(secrets []string) []string {
	out := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret != "" {
			out = append(out, secret)
		}
	}
	return out
}
