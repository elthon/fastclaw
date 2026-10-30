package agent

import "github.com/fastclaw-ai/fastclaw/internal/provider"

const (
	earlierImageOmittedText     = "[Earlier image omitted from inline model context]"
	oversizedImagesOmittedText  = "[Some current images were omitted from inline model context because the combined image payload was too large]"
	maxInlineImagesPerModelCall = 4
	maxInlineImagePayloadBytes  = 6 * 1024 * 1024
)

// modelMessagesWithRecentImages bounds multimodal request growth without
// changing the persisted/archive history used by the chat UI. Providers only
// need the most recent image-bearing user turn for natural follow-ups such as
// "look again"; replaying every older data URL on every turn makes the JSON
// body grow without bound and can trip upstream proxy limits.
//
// Text parts and attachment breadcrumbs remain intact, so an agent can still
// reopen an older workspace image with its tools when the user refers to it
// explicitly. The input slice and its messages are never mutated.
func modelMessagesWithRecentImages(messages []provider.Message) []provider.Message {
	latestImageTurn := -1
	for i, message := range messages {
		if message.Role == "user" && messageHasInlineImage(message) {
			latestImageTurn = i
		}
	}
	if latestImageTurn < 0 {
		return messages
	}

	var result []provider.Message
	inlineImageCount := 0
	inlineImageBytes := 0
	for i := 0; i <= latestImageTurn; i++ {
		message := messages[i]
		if message.Role != "user" || !messageHasInlineImage(message) {
			continue
		}

		parts := make([]provider.ContentPart, 0, len(message.ContentParts))
		omitted := 0
		for _, part := range message.ContentParts {
			if part.Type != "image_url" || part.ImageURL == nil || part.ImageURL.URL == "" {
				parts = append(parts, part)
				continue
			}
			urlBytes := len(part.ImageURL.URL)
			keep := i == latestImageTurn &&
				inlineImageCount < maxInlineImagesPerModelCall &&
				inlineImageBytes+urlBytes <= maxInlineImagePayloadBytes
			if !keep {
				omitted++
				continue
			}
			parts = append(parts, part)
			inlineImageCount++
			inlineImageBytes += urlBytes
		}
		if omitted == 0 {
			continue
		}
		if result == nil {
			result = make([]provider.Message, len(messages))
			copy(result, messages)
		}
		if i == latestImageTurn {
			parts = append(parts, provider.ContentPart{Type: "text", Text: oversizedImagesOmittedText})
		}
		message.ContentParts = parts
		if len(parts) == 0 && message.Content == "" {
			message.Content = earlierImageOmittedText
		}
		result[i] = message
	}
	if result == nil {
		return messages
	}
	return result
}

func messageHasInlineImage(message provider.Message) bool {
	for _, part := range message.ContentParts {
		if part.Type == "image_url" && part.ImageURL != nil && part.ImageURL.URL != "" {
			return true
		}
	}
	return false
}
