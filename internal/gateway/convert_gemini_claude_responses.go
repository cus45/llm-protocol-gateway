package gateway

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// Gemini 请求接到 Claude / Responses 上游：先把 Gemini 请求转成 Chat 的中间
// 形状（convert_gemini_openai.go 的内核），再交给既有的 Chat→Claude /
// Chat→Responses 代理，回包时经同样的中间形状转回 Gemini。复用中间形状意味着
// 工具名、停止词、usage 的规则只有一份。

func (s *Server) proxyGeminiToClaudeMessages(w http.ResponseWriter, r *http.Request, provider domain.Provider, model string, geminiReq map[string]any, skipIncomingAuth bool) (int, TokenUsage, []byte, error) {
	chatReq, err := geminiToOpenAIChatRequest(geminiReq, model)
	if err != nil {
		return 0, TokenUsage{}, nil, err
	}
	if stream, _ := geminiReq["stream"].(bool); stream {
		chatReq["stream"] = true
	}
	return s.proxyConvertedThroughClaude(w, r, provider, model, chatReq, skipIncomingAuth,
		func(claudeBody []byte, model string) ([]byte, TokenUsage, error) {
			// Claude → Chat → Gemini, each hop spelled by the converter that
			// already owns that pair.
			chatBody, usage, err := claudeResponseToOpenAIChat(claudeBody, model, nil)
			if err != nil {
				return nil, TokenUsage{}, err
			}
			out, err := chatJSONToGeminiBody(chatBody, model)
			return out, usage, err
		},
		func(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
			return streamTwoHop(w, reader, model,
				func(w http.ResponseWriter, upstream io.Reader, model string) (TokenUsage, error) {
					return streamClaudeToOpenAIChatEvents(w, upstream, model, nil)
				},
				streamOpenAIChatToGeminiEvents,
			)
		},
	)
}

func (s *Server) proxyGeminiToOpenAIResponses(w http.ResponseWriter, r *http.Request, provider domain.Provider, model string, geminiReq map[string]any, skipIncomingAuth bool) (int, TokenUsage, []byte, error) {
	chatReq, err := geminiToOpenAIChatRequest(geminiReq, model)
	if err != nil {
		return 0, TokenUsage{}, nil, err
	}
	if stream, _ := geminiReq["stream"].(bool); stream {
		chatReq["stream"] = true
	}
	return s.proxyConvertedThroughResponses(w, r, provider, model, geminiChatAsResponsesRequest(chatReq, model), skipIncomingAuth,
		func(responsesBody []byte, model string) ([]byte, TokenUsage, error) {
			chatBody, usage, err := responsesToOpenAIChatResponse(responsesBody, model)
			if err != nil {
				return nil, TokenUsage{}, err
			}
			out, err := chatJSONToGeminiBody(chatBody, model)
			return out, usage, err
		},
		func(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
			return streamTwoHop(w, reader, model,
				func(w http.ResponseWriter, upstream io.Reader, model string) (TokenUsage, error) {
					return streamResponsesToOpenAIChatEvents(w, upstream, model)
				},
				streamOpenAIChatToGeminiEvents,
			)
		},
	)
}

// geminiChatAsResponsesRequest turns the Chat intermediate into the Responses
// request shape, so the Gemini→Responses leg reuses the Responses proxy as it
// is.
func geminiChatAsResponsesRequest(chatReq map[string]any, model string) map[string]any {
	req := map[string]any{"model": model}
	if messages, ok := chatReq["messages"].([]any); ok {
		req["input"] = messages
	}
	for _, key := range []string{"max_tokens", "temperature", "top_p", "tools", "tool_choice", "stream"} {
		if value, exists := chatReq[key]; exists {
			req[key] = value
		}
	}
	return req
}

// chatJSONToGeminiBody turns a Chat completion body into Gemini's response.
func chatJSONToGeminiBody(chatBody []byte, model string) ([]byte, error) {
	var parsed map[string]any
	if err := json.Unmarshal(chatBody, &parsed); err != nil {
		return nil, err
	}
	gemini, err := openAIChatToGeminiResponse(parsed, model)
	if err != nil {
		return nil, err
	}
	return json.Marshal(gemini)
}
