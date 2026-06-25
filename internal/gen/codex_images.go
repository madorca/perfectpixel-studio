package gen

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultCodexImagesBaseURL = "https://chatgpt.com/backend-api/codex"
	defaultCodexRefreshURL    = "https://auth.openai.com/oauth/token"
)

// CodexAuthState is the subset of ~/.codex/auth.json needed for Codex image calls.
type CodexAuthState struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
}

// CodexImages calls the ChatGPT Codex Images backend with the local Codex OAuth session.
type CodexImages struct {
	Model string
	HTTP  *http.Client

	authPath        string
	baseURL         string
	refreshEndpoint string
}

func NewCodexImages(model string) *CodexImages {
	if model == "" {
		model = DefaultModelFor(ProviderCodex)
	}
	return &CodexImages{
		Model: model,
		HTTP:  &http.Client{Timeout: 300 * time.Second},
	}
}

func defaultCodexAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

func loadCodexAuthState(path string) (CodexAuthState, error) {
	if path == "" {
		var err error
		path, err = defaultCodexAuthPath()
		if err != nil {
			return CodexAuthState{}, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CodexAuthState{}, errors.New("Codex login not found. Run `codex login` and try again")
		}
		return CodexAuthState{}, err
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return CodexAuthState{}, fmt.Errorf("read Codex auth file: %w", err)
	}
	src := raw
	if tokens, ok := raw["tokens"].(map[string]any); ok {
		src = tokens
	}

	state := CodexAuthState{
		AccessToken:  stringField(src, "access_token"),
		RefreshToken: stringField(src, "refresh_token"),
		AccountID:    firstNonEmptyString(stringField(src, "account_id"), stringField(raw, "account_id")),
	}
	if state.AccountID == "" {
		state.AccountID = accountIDFromIDToken(stringField(src, "id_token"))
	}
	if state.AccessToken == "" {
		return CodexAuthState{}, errors.New("Codex access token not found. Run `codex login` and try again")
	}
	return state, nil
}

func CodexAuthAvailable() bool {
	state, err := loadCodexAuthState("")
	return err == nil && state.AccessToken != ""
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func accountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	for _, key := range []string{"https://api.openai.com/auth", "account_id", "org_id"} {
		if s := stringField(claims, key); s != "" {
			return s
		}
	}
	if accounts, ok := claims["accounts"].([]any); ok && len(accounts) > 0 {
		if acct, ok := accounts[0].(map[string]any); ok {
			return firstNonEmptyString(stringField(acct, "account_id"), stringField(acct, "id"))
		}
	}
	return ""
}

func (c *CodexImages) GenerateImage(ctx context.Context, prompt string, refImages [][]byte, aspectRatio string) ([]byte, error) {
	fullPrompt := prompt + "\n\n" + aspectHint(aspectRatio)
	size := openAISizeFor(aspectRatio)

	var body []byte
	var contentType string
	var endpoint string
	var err error
	if len(refImages) == 0 {
		body, err = json.Marshal(openAIImageRequest{
			Model:        c.Model,
			Prompt:       fullPrompt,
			N:            1,
			Size:         size,
			Quality:      "medium",
			OutputFormat: "png",
		})
		contentType = "application/json"
		endpoint = c.endpoint("/images/generations")
	} else {
		body, err = c.buildEditBody(fullPrompt, refImages, size)
		contentType = "application/json"
		endpoint = c.endpoint("/images/edits")
	}
	if err != nil {
		return nil, fmt.Errorf("build Codex Images request: %w", err)
	}

	state, err := loadCodexAuthState(c.authPath)
	if err != nil {
		return nil, err
	}

	img, retryable, err := c.doImageRequest(ctx, endpoint, contentType, body, state)
	if err == nil {
		return img, nil
	}
	if !retryable {
		return nil, err
	}
	refreshed, refreshErr := c.refreshAuthState(ctx, state)
	if refreshErr != nil {
		return nil, err
	}
	img, _, err = c.doImageRequest(ctx, endpoint, contentType, body, refreshed)
	return img, err
}

func (c *CodexImages) ValidateKey(ctx context.Context) error {
	state, err := loadCodexAuthState(c.authPath)
	if err != nil {
		return err
	}
	if state.AccessToken == "" {
		return errors.New("Codex access token not found. Run `codex login` and try again")
	}
	return nil
}

func (c *CodexImages) endpoint(path string) string {
	base := c.baseURL
	if base == "" {
		base = defaultCodexImagesBaseURL
	}
	return strings.TrimRight(base, "/") + path
}

func (c *CodexImages) buildEditBody(prompt string, refImages [][]byte, size string) ([]byte, error) {
	images := make([]map[string]string, 0, len(refImages))
	for _, img := range refImages {
		images = append(images, map[string]string{
			"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(img),
		})
	}
	return json.Marshal(map[string]any{
		"model":         c.Model,
		"prompt":        prompt,
		"n":             1,
		"size":          size,
		"quality":       "medium",
		"output_format": "png",
		"images":        images,
	})
}

func (c *CodexImages) doImageRequest(ctx context.Context, endpoint, contentType string, body []byte, state CodexAuthState) (img []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	c.setCodexHeaders(req, state)
	req.Header.Set("Content-Type", contentType)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("Codex Images network error: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, true, fmt.Errorf("read Codex Images response: %w", err)
	}

	var parsed openAIImageResponse
	_ = json.Unmarshal(respBytes, &parsed)

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, true, errors.New("Codex session expired")
		}
		retryable = resp.StatusCode == 429 || resp.StatusCode >= 500
		if parsed.Error != nil && parsed.Error.Message != "" {
			return nil, retryable, fmt.Errorf("Codex Images error (%d): %s", resp.StatusCode, parsed.Error.Message)
		}
		return nil, retryable, fmt.Errorf("Codex Images error (HTTP %d)", resp.StatusCode)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, true, fmt.Errorf("Codex Images error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		return nil, true, errors.New("Codex Images response did not include an image")
	}
	d := parsed.Data[0]
	if d.B64JSON != "" {
		data, err := base64.StdEncoding.DecodeString(d.B64JSON)
		if err != nil {
			return nil, false, fmt.Errorf("decode Codex Images result: %w", err)
		}
		return data, false, nil
	}
	if d.URL != "" {
		data, err := decodeDataOrDownload(c.HTTP, d.URL)
		if err != nil {
			return nil, false, err
		}
		return data, false, nil
	}
	return nil, true, errors.New("Codex Images response did not include an image")
}

func (c *CodexImages) setCodexHeaders(req *http.Request, state CodexAuthState) {
	req.Header.Set("Authorization", "Bearer "+state.AccessToken)
	if state.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", state.AccountID)
	}
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("User-Agent", "codex-tui/0.118.0 Codex Desktop")
	req.Header.Set("Session_id", randomID())
	req.Header.Set("X-Client-Request-Id", randomID())
}

func (c *CodexImages) refreshAuthState(ctx context.Context, state CodexAuthState) (CodexAuthState, error) {
	if state.RefreshToken == "" {
		return CodexAuthState{}, errors.New("Codex refresh token not found. Run `codex login` and try again")
	}
	endpoint := c.refreshEndpoint
	if endpoint == "" {
		endpoint = defaultCodexRefreshURL
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", state.RefreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return CodexAuthState{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return CodexAuthState{}, fmt.Errorf("refresh Codex session: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return CodexAuthState{}, err
	}
	if resp.StatusCode != http.StatusOK {
		if strings.Contains(string(data), "refresh_token_reused") {
			return CodexAuthState{}, errors.New("Codex login expired. Run `codex logout`, then `codex login`")
		}
		return CodexAuthState{}, fmt.Errorf("refresh Codex session failed (HTTP %d)", resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return CodexAuthState{}, err
	}
	next := state
	if access := stringField(parsed, "access_token"); access != "" {
		next.AccessToken = access
	}
	if refresh := stringField(parsed, "refresh_token"); refresh != "" {
		next.RefreshToken = refresh
	}
	if err := c.saveRefreshedAuthState(next); err != nil {
		return CodexAuthState{}, err
	}
	return next, nil
}

func (c *CodexImages) saveRefreshedAuthState(state CodexAuthState) error {
	path := c.authPath
	if path == "" {
		var err error
		path, err = defaultCodexAuthPath()
		if err != nil {
			return err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	tokens, ok := raw["tokens"].(map[string]any)
	if !ok {
		tokens = raw
	} else {
		raw["tokens"] = tokens
	}
	tokens["access_token"] = state.AccessToken
	tokens["refresh_token"] = state.RefreshToken
	if state.AccountID != "" {
		tokens["account_id"] = state.AccountID
	}
	raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339)

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
