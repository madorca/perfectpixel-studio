// Package config는 앱 설정 영속화를 담당합니다.
package config

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ProviderCfg는 프로바이더별 키/모델 설정입니다.
type ProviderCfg struct {
	APIKey string `json:"apiKey"`
	Model  string `json:"model"`
}

// Settings는 사용자 설정입니다.
type Settings struct {
	Provider   string      `json:"provider"` // gemini | openai | openrouter | fal | byteplus
	Gemini     ProviderCfg `json:"gemini"`
	OpenAI     ProviderCfg `json:"openai"`
	Codex      ProviderCfg `json:"codex"`
	OpenRouter ProviderCfg `json:"openrouter"`
	Fal        ProviderCfg `json:"fal"`
	BytePlus   ProviderCfg `json:"byteplus"`

	// 레거시 필드 (v1 → 마이그레이션용)
	LegacyAPIKey string `json:"apiKey,omitempty"`
	LegacyModel  string `json:"model,omitempty"`
}

// Cfg는 프로바이더 이름으로 해당 설정을 반환합니다.
func (s *Settings) Cfg(provider string) *ProviderCfg {
	switch provider {
	case "openai":
		return &s.OpenAI
	case "codex":
		return &s.Codex
	case "openrouter":
		return &s.OpenRouter
	case "fal":
		return &s.Fal
	case "byteplus":
		return &s.BytePlus
	default:
		return &s.Gemini
	}
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "perfectpixel", "config.json"), nil
}

// SessionPath는 작업 세션 스냅샷 파일 경로를 반환합니다.
func SessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "perfectpixel", "session.json"), nil
}

// GalleryDir는 생성 이미지가 자동 보관되는 갤러리 디렉토리 경로를 반환합니다.
func GalleryDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "perfectpixel", "gallery"), nil
}

// Load는 설정을 읽고 레거시 마이그레이션 및 환경변수 폴백을 적용합니다.
func Load() Settings {
	var s Settings
	if path, err := configPath(); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &s)
		}
	}

	// v1 단일 키 → Gemini로 마이그레이션
	if s.LegacyAPIKey != "" && s.Gemini.APIKey == "" {
		s.Gemini.APIKey = s.LegacyAPIKey
		if s.LegacyModel != "" {
			s.Gemini.Model = s.LegacyModel
		}
	}
	s.LegacyAPIKey = ""
	s.LegacyModel = ""

	// 환경변수 / .env 파일 폴백 (설정 파일이 우선)
	env := loadEnvFallback()
	if s.Gemini.APIKey == "" {
		s.Gemini.APIKey = firstNonEmpty(env["GEMINI_API_KEY"], env["GOOGLE_API_KEY"])
	}
	if s.OpenAI.APIKey == "" {
		s.OpenAI.APIKey = env["OPENAI_API_KEY"]
	}
	if s.OpenRouter.APIKey == "" {
		s.OpenRouter.APIKey = env["OPENROUTER_API_KEY"]
	}
	if s.Fal.APIKey == "" {
		s.Fal.APIKey = firstNonEmpty(env["FAL_KEY"], env["FAL_API_KEY"])
	}
	if s.BytePlus.APIKey == "" {
		s.BytePlus.APIKey = firstNonEmpty(env["BYTEPLUS_API_KEY"], env["ARK_API_KEY"])
	}

	// 활성 프로바이더 자동 선택: 키가 있는 첫 프로바이더
	if s.Provider == "" {
		switch {
		case codexAuthAvailable():
			s.Provider = "codex"
		case s.Gemini.APIKey != "":
			s.Provider = "gemini"
		case s.OpenAI.APIKey != "":
			s.Provider = "openai"
		case s.OpenRouter.APIKey != "":
			s.Provider = "openrouter"
		case s.Fal.APIKey != "":
			s.Provider = "fal"
		case s.BytePlus.APIKey != "":
			s.Provider = "byteplus"
		default:
			s.Provider = "gemini"
		}
	}
	return s
}

func codexAuthAvailable() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return false
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	src := raw
	if tokens, ok := raw["tokens"].(map[string]any); ok {
		src = tokens
	}
	token, _ := src["access_token"].(string)
	return token != ""
}

// Save는 설정을 저장합니다 (0600 권한).
func Save(s Settings) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// loadEnvFallback은 OS 환경변수와 실행 위치 주변의 .env/.env.local을 읽습니다.
func loadEnvFallback() map[string]string {
	out := map[string]string{}

	// 1) .env 파일 (작업 디렉토리 + 실행 파일 디렉토리)
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range dirs {
		for _, name := range []string{".env", ".env.local"} {
			parseEnvFile(filepath.Join(dir, name), out)
		}
	}

	// 2) OS 환경변수 (파일보다 우선)
	for _, key := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "FAL_KEY", "FAL_API_KEY", "BYTEPLUS_API_KEY", "ARK_API_KEY"} {
		if v := os.Getenv(key); v != "" {
			out[key] = v
		}
	}
	return out
}

func parseEnvFile(path string, out map[string]string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && v != "" {
			out[k] = v
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
