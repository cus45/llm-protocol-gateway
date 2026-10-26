package gateway

import (
	"encoding/json"
	"strings"
)

// quotaPromptEstimate sizes the reservation a request's prompt makes against its
// key's token quota, from the raw body.
//
// Media payloads do not count. The estimator reads about four characters to a
// token, and a base64 image is four characters per three bytes: a 1 MiB
// screenshot would reserve half a million tokens for something the model charges
// about a thousand for, and a key with a sane quota would refuse every request
// that carries one. The part stays in the body (the estimate still sees that a
// media part is there), only its bytes are blanked.
func quotaPromptEstimate(body []byte) int64 {
	return estimateRTKTokens(withoutBinaryPayloads(string(body)))
}

// withoutBinaryPayloads blanks the base64 data of inline media in a JSON body:
// a Gemini inlineData / fileData part, or any string that is itself a data: URL.
// A body that does not parse, or carries nothing of the kind, comes back as it
// was.
func withoutBinaryPayloads(text string) string {
	if !strings.Contains(text, "base64,") && !strings.Contains(text, "inlineData") && !strings.Contains(text, "fileData") {
		return text
	}
	var document any
	if json.Unmarshal([]byte(text), &document) != nil {
		return text
	}
	stripBinaryPayloads(document)
	encoded, err := json.Marshal(document)
	if err != nil {
		return text
	}
	return string(encoded)
}

// stripBinaryPayloads walks a decoded body in place.
func stripBinaryPayloads(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"inlineData", "fileData"} {
			part, ok := typed[key].(map[string]any)
			if !ok {
				continue
			}
			if _, hasData := part["data"]; hasData {
				part["data"] = ""
			}
		}
		for key, child := range typed {
			if text, ok := child.(string); ok {
				if marker := strings.Index(text, ";base64,"); strings.HasPrefix(text, "data:") && marker >= 0 {
					typed[key] = text[:marker+len(";base64,")]
					continue
				}
			}
			stripBinaryPayloads(child)
		}
	case []any:
		for _, child := range typed {
			stripBinaryPayloads(child)
		}
	}
}
