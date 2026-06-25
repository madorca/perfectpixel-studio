package gen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCodexAuthFile(t *testing.T, dir string, accessToken, refreshToken, accountID string) string {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	body := map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"account_id":    accountID,
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCodexAuthStateReadsTokensObject(t *testing.T) {
	path := writeCodexAuthFile(t, t.TempDir(), "access-1", "refresh-1", "acct-1")

	state, err := loadCodexAuthState(path)
	if err != nil {
		t.Fatalf("loadCodexAuthState failed: %v", err)
	}

	if state.AccessToken != "access-1" {
		t.Fatalf("access token mismatch: %q", state.AccessToken)
	}
	if state.RefreshToken != "refresh-1" {
		t.Fatalf("refresh token mismatch: %q", state.RefreshToken)
	}
	if state.AccountID != "acct-1" {
		t.Fatalf("account id mismatch: %q", state.AccountID)
	}
}

func TestCodexImagesGenerateUsesLocalOAuthHeaders(t *testing.T) {
	authPath := writeCodexAuthFile(t, t.TempDir(), "access-1", "refresh-1", "acct-1")
	imageBytes := []byte("png-bytes")

	var gotAuth, gotAccount, gotOriginator, gotSessionID, gotRequestID string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("Chatgpt-Account-Id")
		gotOriginator = r.Header.Get("Originator")
		gotSessionID = r.Header.Get("Session_id")
		gotRequestID = r.Header.Get("X-Client-Request-Id")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(openAIImageResponse{
			Data: []struct {
				B64JSON string `json:"b64_json"`
				URL     string `json:"url"`
			}{{B64JSON: base64.StdEncoding.EncodeToString(imageBytes)}},
		})
	}))
	defer srv.Close()

	c := NewCodexImages("gpt-image-2")
	c.HTTP = srv.Client()
	c.authPath = authPath
	c.baseURL = srv.URL

	got, err := c.GenerateImage(context.Background(), "draw a cat", nil, "1:1")
	if err != nil {
		t.Fatalf("GenerateImage failed: %v", err)
	}

	if string(got) != string(imageBytes) {
		t.Fatalf("image bytes mismatch: %q", got)
	}
	if gotAuth != "Bearer access-1" {
		t.Fatalf("Authorization header mismatch: %q", gotAuth)
	}
	if gotAccount != "acct-1" {
		t.Fatalf("account header mismatch: %q", gotAccount)
	}
	if gotOriginator != "codex-tui" {
		t.Fatalf("Originator header mismatch: %q", gotOriginator)
	}
	if gotSessionID == "" || gotRequestID == "" {
		t.Fatalf("request ids should be set, session=%q request=%q", gotSessionID, gotRequestID)
	}
	if gotBody["model"] != "gpt-image-2" {
		t.Fatalf("model mismatch: %#v", gotBody["model"])
	}
	if !strings.Contains(gotBody["prompt"].(string), "draw a cat") {
		t.Fatalf("prompt missing user text: %#v", gotBody["prompt"])
	}
}

func TestCodexImagesEditSendsReferenceImagesAsJSONDataURLs(t *testing.T) {
	authPath := writeCodexAuthFile(t, t.TempDir(), "access-1", "refresh-1", "acct-1")
	imageBytes := []byte("edited-png")

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Codex Images edit must use JSON body, got Content-Type %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if gotBody["model"] != "gpt-image-2" {
			t.Fatalf("model mismatch: %#v", gotBody["model"])
		}
		if gotBody["output_format"] != "png" {
			t.Fatalf("output_format mismatch: %#v", gotBody["output_format"])
		}
		images, ok := gotBody["images"].([]any)
		if !ok || len(images) != 1 {
			t.Fatalf("expected one JSON image reference, got %#v", gotBody["images"])
		}
		imageRef, ok := images[0].(map[string]any)
		if !ok {
			t.Fatalf("expected image reference object, got %#v", images[0])
		}
		wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("base-png"))
		if imageRef["image_url"] != wantURL {
			t.Fatalf("image_url mismatch: %#v", imageRef["image_url"])
		}
		_ = json.NewEncoder(w).Encode(openAIImageResponse{
			Data: []struct {
				B64JSON string `json:"b64_json"`
				URL     string `json:"url"`
			}{{B64JSON: base64.StdEncoding.EncodeToString(imageBytes)}},
		})
	}))
	defer srv.Close()

	c := NewCodexImages("gpt-image-2")
	c.HTTP = srv.Client()
	c.authPath = authPath
	c.baseURL = srv.URL

	got, err := c.GenerateImage(context.Background(), "make a run strip", [][]byte{[]byte("base-png")}, "16:9")
	if err != nil {
		t.Fatalf("GenerateImage failed: %v", err)
	}
	if string(got) != string(imageBytes) {
		t.Fatalf("image bytes mismatch: %q", got)
	}
}

func TestCodexImagesRefreshesOAuthAfterUnauthorized(t *testing.T) {
	authPath := writeCodexAuthFile(t, t.TempDir(), "expired-access", "refresh-1", "acct-1")
	imageBytes := []byte("fresh-png")

	var imageCalls int
	imageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageCalls++
		switch imageCalls {
		case 1:
			if got := r.Header.Get("Authorization"); got != "Bearer expired-access" {
				t.Fatalf("first Authorization header mismatch: %q", got)
			}
			http.Error(w, `{"error":{"message":"expired"}}`, http.StatusUnauthorized)
		case 2:
			if got := r.Header.Get("Authorization"); got != "Bearer fresh-access" {
				t.Fatalf("second Authorization header mismatch: %q", got)
			}
			_ = json.NewEncoder(w).Encode(openAIImageResponse{
				Data: []struct {
					B64JSON string `json:"b64_json"`
					URL     string `json:"url"`
				}{{B64JSON: base64.StdEncoding.EncodeToString(imageBytes)}},
			})
		default:
			t.Fatalf("unexpected image call %d", imageCalls)
		}
	}))
	defer imageSrv.Close()

	var refreshCalls int
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse refresh form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type mismatch: %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "refresh-1" {
			t.Fatalf("refresh_token mismatch: %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "refresh-2",
		})
	}))
	defer refreshSrv.Close()

	c := NewCodexImages("gpt-image-2")
	c.HTTP = imageSrv.Client()
	c.authPath = authPath
	c.baseURL = imageSrv.URL
	c.refreshEndpoint = refreshSrv.URL

	got, err := c.GenerateImage(context.Background(), "draw a dog", nil, "1:1")
	if err != nil {
		t.Fatalf("GenerateImage failed: %v", err)
	}

	if string(got) != string(imageBytes) {
		t.Fatalf("image bytes mismatch: %q", got)
	}
	if imageCalls != 2 {
		t.Fatalf("image call count mismatch: %d", imageCalls)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh call count mismatch: %d", refreshCalls)
	}

	state, err := loadCodexAuthState(authPath)
	if err != nil {
		t.Fatalf("reload auth state: %v", err)
	}
	if state.AccessToken != "fresh-access" || state.RefreshToken != "refresh-2" {
		t.Fatalf("auth file was not refreshed: %#v", state)
	}
}
