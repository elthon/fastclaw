package agent

import (
	"reflect"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

func inlineImage(url string) provider.ContentPart {
	return provider.ContentPart{
		Type:     "image_url",
		ImageURL: &provider.ImageURL{URL: url},
	}
}

func TestModelMessagesWithRecentImagesKeepsOnlyLatestImageTurn(t *testing.T) {
	messages := []provider.Message{
		{
			Role: "user",
			ContentParts: []provider.ContentPart{
				{Type: "text", Text: "[Attached: /workspace/old.png]\nold"},
				inlineImage("data:image/png;base64,OLD"),
			},
		},
		{Role: "assistant", Content: "old reply"},
		{
			Role: "user",
			ContentParts: []provider.ContentPart{
				{Type: "text", Text: "[Attached: /workspace/new.png]\nnew"},
				inlineImage("data:image/png;base64,NEW"),
			},
		},
		{Role: "assistant", Content: "new reply"},
		{Role: "user", Content: "look again"},
	}
	original := append([]provider.ContentPart(nil), messages[0].ContentParts...)

	got := modelMessagesWithRecentImages(messages)
	if messageHasInlineImage(got[0]) {
		t.Fatal("older image turn still contains an inline image")
	}
	if len(got[0].ContentParts) != 1 || got[0].ContentParts[0].Text == "" {
		t.Fatalf("older image turn lost its text breadcrumb: %#v", got[0])
	}
	if !messageHasInlineImage(got[2]) {
		t.Fatal("latest image turn was stripped")
	}
	if !reflect.DeepEqual(messages[0].ContentParts, original) {
		t.Fatal("input history was mutated")
	}
}

func TestModelMessagesWithRecentImagesKeepsImageForTextFollowUp(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", ContentParts: []provider.ContentPart{inlineImage("data:image/png;base64,ONE")}},
		{Role: "assistant", Content: "description"},
		{Role: "user", Content: "what about the icon?"},
	}

	got := modelMessagesWithRecentImages(messages)
	if !messageHasInlineImage(got[0]) {
		t.Fatal("the most recent image must remain available to a text-only follow-up")
	}
}

func TestModelMessagesWithRecentImagesReplacesImageOnlyOlderTurn(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", ContentParts: []provider.ContentPart{inlineImage("data:image/png;base64,OLD")}},
		{Role: "assistant", Content: "first"},
		{Role: "user", ContentParts: []provider.ContentPart{inlineImage("data:image/png;base64,NEW")}},
	}

	got := modelMessagesWithRecentImages(messages)
	if got[0].Content != earlierImageOmittedText || len(got[0].ContentParts) != 0 {
		t.Fatalf("image-only older turn was not replaced safely: %#v", got[0])
	}
	if !messageHasInlineImage(got[2]) {
		t.Fatal("latest image turn was stripped")
	}
}

func TestModelMessagesWithRecentImagesCapsCurrentImagePayload(t *testing.T) {
	largeURL := "data:image/png;base64," + string(make([]byte, maxInlineImagePayloadBytes))
	messages := []provider.Message{{
		Role: "user",
		ContentParts: []provider.ContentPart{
			inlineImage("data:image/png;base64,ONE"),
			inlineImage(largeURL),
		},
	}}

	got := modelMessagesWithRecentImages(messages)
	imageCount := 0
	foundOmissionNote := false
	for _, part := range got[0].ContentParts {
		if part.Type == "image_url" {
			imageCount++
		}
		if part.Type == "text" && part.Text == oversizedImagesOmittedText {
			foundOmissionNote = true
		}
	}
	if imageCount != 1 || !foundOmissionNote {
		t.Fatalf("payload cap result = %#v; want one image plus omission note", got[0].ContentParts)
	}
}

func TestModelMessagesWithRecentImagesCapsImageCount(t *testing.T) {
	parts := make([]provider.ContentPart, 0, maxInlineImagesPerModelCall+1)
	for i := 0; i < maxInlineImagesPerModelCall+1; i++ {
		parts = append(parts, inlineImage("https://example.com/image.png"))
	}

	got := modelMessagesWithRecentImages([]provider.Message{{Role: "user", ContentParts: parts}})
	imageCount := 0
	for _, part := range got[0].ContentParts {
		if part.Type == "image_url" {
			imageCount++
		}
	}
	if imageCount != maxInlineImagesPerModelCall {
		t.Fatalf("kept %d images, want %d", imageCount, maxInlineImagesPerModelCall)
	}
}
