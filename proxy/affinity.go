package proxy

import (
	"net/http"
	"strings"
)

func requestAffinityKey(r *http.Request, payload *KiroPayload) string {
	if r != nil {
		for _, header := range []string{
			"X-Session-ID",
			"X-Conversation-ID",
			"X-Thread-ID",
			"X-Amp-Thread-Id",
			"X-Kiro-Conversation-ID",
		} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				return "header:" + strings.ToLower(header) + ":" + value
			}
		}
	}
	if payload != nil {
		if id := strings.TrimSpace(payload.ConversationState.ConversationID); id != "" {
			return "conversation:" + id
		}
	}
	return ""
}
