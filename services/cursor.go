package services

import (
	"context"
	"cursor2api-go/config"
	"cursor2api-go/middleware"
	"cursor2api-go/models"
	"cursor2api-go/utils"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
	"github.com/sirupsen/logrus"
)

const cursorAPIURL = "https://cursor.com/api/chat"

// CursorService handles interactions with Cursor API.
type CursorService struct {
	config          *config.Config
	client          *req.Client
	mainJS          string
	envJS           string
	scriptCache     string
	scriptCacheTime time.Time
	scriptMutex     sync.RWMutex
	headerGenerator *utils.HeaderGenerator
}

// NewCursorService creates a new service instance.
func NewCursorService(cfg *config.Config) *CursorService {
	mainJS, err := os.ReadFile(filepath.Join("jscode", "main.js"))
	if err != nil {
		logrus.Fatalf("failed to read jscode/main.js: %v", err)
	}

	envJS, err := os.ReadFile(filepath.Join("jscode", "env.js"))
	if err != nil {
		logrus.Fatalf("failed to read jscode/env.js: %v", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		logrus.Warnf("failed to create cookie jar: %v", err)
	}

	client := req.C()
	client.SetTimeout(time.Duration(cfg.Timeout) * time.Second)
	client.ImpersonateChrome()
	if jar != nil {
		client.SetCookieJar(jar)
	}

	return &CursorService{
		config:          cfg,
		client:          client,
		mainJS:          string(mainJS),
		envJS:           string(envJS),
		headerGenerator: utils.NewHeaderGenerator(),
	}
}

// ChatCompletion creates a chat completion stream for the given request.
func (s *CursorService) ChatCompletion(ctx context.Context, request *models.ChatCompletionRequest) (<-chan interface{}, error) {
	truncatedMessages := s.truncateMessages(request.Messages)
	cursorMessages := models.ToCursorMessages(truncatedMessages, s.config.SystemPromptInject)

	// 转换模型名称为 Cursor API 格式
	cursorModel := s.convertModelName(request.Model)
	
	payload := models.CursorRequest{
		Context:  []interface{}{},
		Model:    cursorModel,
		ID:       utils.GenerateRandomString(16),
		Messages: cursorMessages,
		Trigger:  "submit-message",
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cursor payload: %w", err)
	}

	// 尝试最多2次
	maxRetries := 2
	for attempt := 1; attempt <= maxRetries; attempt++ {
		xIsHuman, err := s.fetchXIsHuman(ctx)
		if err != nil {
			if attempt < maxRetries {
				logrus.WithError(err).Warnf("Failed to fetch x-is-human token (attempt %d/%d), retrying...", attempt, maxRetries)
				time.Sleep(time.Second * time.Duration(attempt)) // 指数退避
				continue
			}
			return nil, err
		}

		// 添加详细的调试日志
		headers := s.chatHeaders(xIsHuman)
		logrus.WithFields(logrus.Fields{
			"url":            cursorAPIURL,
			"x-is-human":     xIsHuman[:50] + "...", // 只显示前50个字符
			"payload_length": len(jsonPayload),
			"model":          request.Model,
			"attempt":        attempt,
		}).Debug("Sending request to Cursor API")

		resp, err := s.client.R().
			SetContext(ctx).
			SetHeaders(headers).
			SetBody(jsonPayload).
			DisableAutoReadResponse().
			Post(cursorAPIURL)
		if err != nil {
			if attempt < maxRetries {
				logrus.WithError(err).Warnf("Cursor request failed (attempt %d/%d), retrying...", attempt, maxRetries)
				time.Sleep(time.Second * time.Duration(attempt))
				continue
			}
			return nil, fmt.Errorf("cursor request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Response.Body)
			resp.Response.Body.Close()
			message := strings.TrimSpace(string(body))

			// 记录详细的错误信息
			logrus.WithFields(logrus.Fields{
				"status_code": resp.StatusCode,
				"response":    message,
				"headers":     resp.Header,
				"attempt":     attempt,
			}).Error("Cursor API returned non-OK status")

			// 如果是 403 错误且还有重试机会,清除缓存并重试
			if resp.StatusCode == http.StatusForbidden && attempt < maxRetries {
				logrus.Warn("Received 403 Access Denied, refreshing browser fingerprint and clearing token cache...")

				// 刷新浏览器指纹
				s.headerGenerator.Refresh()
				logrus.WithFields(logrus.Fields{
					"platform":       s.headerGenerator.GetProfile().Platform,
					"chrome_version": s.headerGenerator.GetProfile().ChromeVersion,
				}).Debug("Refreshed browser fingerprint")

				// 清除 token 缓存
				s.scriptMutex.Lock()
				s.scriptCache = ""
				s.scriptCacheTime = time.Time{}
				s.scriptMutex.Unlock()

				time.Sleep(time.Second * time.Duration(attempt))
				continue
			}

			if strings.Contains(message, "Attention Required! | Cloudflare") {
				message = "Cloudflare 403"
			}
			return nil, middleware.NewCursorWebError(resp.StatusCode, message)
		}

		// 成功,返回结果
		output := make(chan interface{}, 32)
		go s.consumeSSE(ctx, resp.Response, output)
		return output, nil
	}

	return nil, fmt.Errorf("failed after %d attempts", maxRetries)
}

func (s *CursorService) consumeSSE(ctx context.Context, resp *http.Response, output chan interface{}) {
	defer close(output)

	if err := utils.ReadSSEStream(ctx, resp, output); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		errResp := middleware.NewCursorWebError(http.StatusBadGateway, err.Error())
		select {
		case output <- errResp:
		default:
			logrus.WithError(err).Warn("failed to push SSE error to channel")
		}
	}
}

func (s *CursorService) fetchXIsHuman(ctx context.Context) (string, error) {
	// 检查缓存
	s.scriptMutex.RLock()
	cached := s.scriptCache
	lastFetch := s.scriptCacheTime
	s.scriptMutex.RUnlock()

	var scriptBody string
	// 缓存有效期缩短到1分钟,避免 token 过期
	if cached != "" && time.Since(lastFetch) < 1*time.Minute {
		scriptBody = cached
	} else {
		// 如果 SCRIPT_URL 为空或无法访问，使用空字符串（降级方案）
		if s.config.ScriptURL == "" {
			logrus.Warn("SCRIPT_URL is empty, using fallback mode")
			scriptBody = ""
		} else {
			resp, err := s.client.R().
				SetContext(ctx).
				SetHeaders(s.scriptHeaders()).
				Get(s.config.ScriptURL)

			if err != nil {
				// 如果请求失败，使用缓存或空字符串
				if cached != "" {
					logrus.Warnf("Failed to fetch script, using cached version: %v", err)
					scriptBody = cached
				} else {
					logrus.Warnf("Failed to fetch script and no cache available, using fallback mode: %v", err)
					scriptBody = "" // 降级方案：使用空字符串
				}
			} else if resp.StatusCode != http.StatusOK {
				// 如果状态码异常，使用缓存或空字符串
				if cached != "" {
					logrus.Warnf("Script fetch returned status %d, using cached version", resp.StatusCode)
					scriptBody = cached
				} else {
					logrus.Warnf("Script fetch returned status %d and no cache available, using fallback mode", resp.StatusCode)
					scriptBody = "" // 降级方案
				}
			} else {
				scriptBody = string(resp.Bytes())
				// 更新缓存
				s.scriptMutex.Lock()
				s.scriptCache = scriptBody
				s.scriptCacheTime = time.Now()
				s.scriptMutex.Unlock()
			}
		}
	}

	compiled := s.prepareJS(scriptBody)
	value, err := utils.RunJS(compiled)
	if err != nil {
		// JS 执行失败时清除缓存
		s.scriptMutex.Lock()
		s.scriptCache = ""
		s.scriptCacheTime = time.Time{}
		s.scriptMutex.Unlock()
		return "", fmt.Errorf("failed to execute JS: %w", err)
	}

	logrus.WithField("length", len(value)).Debug("Fetched x-is-human token")

	return value, nil
}

func (s *CursorService) prepareJS(cursorJS string) string {
	replacer := strings.NewReplacer(
		"$$currentScriptSrc$$", s.config.ScriptURL,
		"$$UNMASKED_VENDOR_WEBGL$$", s.config.FP.UNMASKED_VENDOR_WEBGL,
		"$$UNMASKED_RENDERER_WEBGL$$", s.config.FP.UNMASKED_RENDERER_WEBGL,
		"$$userAgent$$", s.config.FP.UserAgent,
	)

	mainScript := replacer.Replace(s.mainJS)
	mainScript = strings.Replace(mainScript, "$$env_jscode$$", s.envJS, 1)
	mainScript = strings.Replace(mainScript, "$$cursor_jscode$$", cursorJS, 1)
	return mainScript
}

func (s *CursorService) truncateMessages(messages []models.Message) []models.Message {
	if len(messages) == 0 || s.config.MaxInputLength <= 0 {
		return messages
	}

	maxLength := s.config.MaxInputLength
	total := 0
	for _, msg := range messages {
		total += len(msg.GetStringContent())
	}

	if total <= maxLength {
		return messages
	}

	var result []models.Message
	startIdx := 0

	if strings.EqualFold(messages[0].Role, "system") {
		result = append(result, messages[0])
		maxLength -= len(messages[0].GetStringContent())
		if maxLength < 0 {
			maxLength = 0
		}
		startIdx = 1
	}

	current := 0
	collected := make([]models.Message, 0, len(messages)-startIdx)
	for i := len(messages) - 1; i >= startIdx; i-- {
		msg := messages[i]
		msgLen := len(msg.GetStringContent())
		if msgLen == 0 {
			continue
		}
		if current+msgLen > maxLength {
			continue
		}
		collected = append(collected, msg)
		current += msgLen
	}

	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}

	return append(result, collected...)
}

func (s *CursorService) chatHeaders(xIsHuman string) map[string]string {
	return s.headerGenerator.GetChatHeaders(xIsHuman)
}

func (s *CursorService) scriptHeaders() map[string]string {
	return s.headerGenerator.GetScriptHeaders()
}

// convertModelName 转换模型名称为 Cursor API 格式
func (s *CursorService) convertModelName(modelName string) string {
	// 如果已经是正确格式（包含 /），直接返回
	if strings.Contains(modelName, "/") {
		return modelName
	}
	
	// 获取模型配置
	if config, exists := models.GetModelConfig(modelName); exists {
		// 如果配置中的 ID 包含 /，使用配置的 ID
		if strings.Contains(config.ID, "/") {
			return config.ID
		}
	}
	
	// 默认映射规则
	switch {
	case strings.Contains(modelName, "claude"):
		if strings.Contains(modelName, "opus") {
			return "anthropic/claude-opus-4.6"
		}
		return "anthropic/claude-sonnet-4.6"
	case strings.Contains(modelName, "gemini"):
		return "google/gemini-3.1-pro"
	case strings.Contains(modelName, "gpt"):
		// GPT 模型映射到 Claude（因为 Cursor 主要支持 Claude）
		return "anthropic/claude-sonnet-4.6"
	default:
		// 默认使用 Sonnet
		return "anthropic/claude-sonnet-4.6"
	}
}
