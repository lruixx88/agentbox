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

const zhipuCompatBaseURL = "https://open.bigmodel.cn/api/paas/v4"

// ZhipuCompatHandler proxies OpenAI-compatible requests to Zhipu and rewrites roles.
type ZhipuCompatHandler struct {
	providerMgr *provider.Manager
}

func NewZhipuCompatHandler(providerMgr *provider.Manager) *ZhipuCompatHandler {
	return &ZhipuCompatHandler{providerMgr: providerMgr}
}

func (h *ZhipuCompatHandler) Proxy(c *gin.Context) {
	path := c.Param("path")
	if path == "" || path == "/" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing path"})
		return
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
		return
	}

	rewritten := bodyBytes
	if len(bodyBytes) > 0 && strings.Contains(string(bodyBytes), "\"messages\"") {
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

	targetURL := zhipuCompatBaseURL + path
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		targetURL += "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL, bytes.NewReader(rewritten))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "create proxy request failed"})
		return
	}

	// Pass through auth and content type.
	auth := c.GetHeader("Authorization")
	if auth == "" {
		auth = h.resolveZhipuAuth()
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if ct := c.GetHeader("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if accept := c.GetHeader("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "proxy request failed"})
		return
	}
	defer resp.Body.Close()

	// Copy response headers (minimal safe set).
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}

func (h *ZhipuCompatHandler) resolveZhipuAuth() string {
	if h.providerMgr != nil {
		if key, _, err := h.providerMgr.GetDecryptedKeyWithRotation("zhipu"); err == nil && key != "" {
			return "Bearer " + key
		}
		for _, p := range h.providerMgr.List() {
			if p.ID == "zhipu" || p.TemplateID == "zhipu" || strings.Contains(p.BaseURL, "open.bigmodel.cn") {
				if key, _, err := h.providerMgr.GetDecryptedKeyWithRotation(p.ID); err == nil && key != "" {
					return "Bearer " + key
				}
			}
		}
	}
	if fallback := os.Getenv("AGENTBOX_ZHIPU_API_KEY"); fallback != "" {
		return "Bearer " + fallback
	}
	return ""
}
