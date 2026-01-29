package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmalldedede/agentbox/internal/provider"
)

const (
	compatModeOpenAI    = "openai"
	compatModeAnthropic = "anthropic"

	defaultOpenAIBase    = "https://api.openai.com/v1"
	defaultAnthropicBase = "https://api.anthropic.com"
)

// CompatHandler proxies requests to provider base URLs and injects auth/compat rewrites.
type CompatHandler struct {
	providerMgr *provider.Manager
}

func NewCompatHandler(providerMgr *provider.Manager) *CompatHandler {
	return &CompatHandler{providerMgr: providerMgr}
}

func (h *CompatHandler) ProxyOpenAI(c *gin.Context) {
	h.proxy(c, compatModeOpenAI)
}

func (h *CompatHandler) ProxyAnthropic(c *gin.Context) {
	h.proxy(c, compatModeAnthropic)
}

func (h *CompatHandler) proxy(c *gin.Context, mode string) {
	providerID := c.Param("provider")
	if providerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing provider"})
		return
	}
	path := c.Param("path")
	if path == "" || path == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing path"})
		return
	}

	p, err := h.providerMgr.Get(providerID)
	if err != nil || p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return
	}

	baseURL := strings.TrimRight(p.BaseURL, "/")
	if baseURL == "" {
		if mode == compatModeAnthropic {
			baseURL = defaultAnthropicBase
		} else {
			baseURL = defaultOpenAIBase
		}
	}

	if mode == compatModeOpenAI {
		if strings.Contains(baseURL, "open.bigmodel.cn") && strings.Contains(baseURL, "/api/anthropic") {
			baseURL = strings.Replace(baseURL, "/api/anthropic", "/api/paas/v4", 1)
		}
	}

	targetURL := baseURL + path
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		targetURL += "?" + rawQuery
	}

	var bodyBytes []byte
	if c.Request.Body != nil {
		bodyBytes, _ = io.ReadAll(c.Request.Body)
	}
	rewritten := bodyBytes
	if mode == compatModeOpenAI && len(bodyBytes) > 0 && strings.Contains(string(bodyBytes), "\"messages\"") {
		var payload map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &payload); err == nil {
			if msgs, ok := payload["messages"].([]interface{}); ok {
				for _, m := range msgs {
					item, ok := m.(map[string]interface{})
					if !ok {
						continue
					}
					if role, ok := item["role"].(string); ok && role == "developer" {
						item["role"] = "system"
					}
				}
				if updated, err := json.Marshal(payload); err == nil {
					rewritten = updated
				}
			}
		}
	}

	var bodyReader io.Reader
	if len(rewritten) > 0 && c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		bodyReader = bytes.NewReader(rewritten)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL, bodyReader)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "create proxy request failed"})
		return
	}

	if ct := c.GetHeader("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if accept := c.GetHeader("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	if mode == compatModeAnthropic {
		if key := h.resolveProviderKey(providerID); key != "" {
			req.Header.Set("x-api-key", key)
		} else if c.GetHeader("x-api-key") != "" {
			req.Header.Set("x-api-key", c.GetHeader("x-api-key"))
		} else if auth := c.GetHeader("Authorization"); auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	} else {
		if key := h.resolveProviderKey(providerID); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		} else if auth := c.GetHeader("Authorization"); auth != "" {
			req.Header.Set("Authorization", auth)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "proxy request failed"})
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}

func (h *CompatHandler) resolveProviderKey(providerID string) string {
	if h.providerMgr != nil {
		if key, _, err := h.providerMgr.GetDecryptedKeyWithRotation(providerID); err == nil && key != "" {
			return key
		}
	}

	upper := strings.ToUpper(strings.ReplaceAll(providerID, "-", "_"))
	if key := os.Getenv("AGENTBOX_" + upper + "_API_KEY"); key != "" {
		return key
	}
	if key := os.Getenv("AGENTBOX_PROVIDER_API_KEY"); key != "" {
		return key
	}
	return ""
}
