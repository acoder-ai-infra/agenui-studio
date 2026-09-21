package modelgateway

// estimatePromptInput keeps binary transport size separate from model context
// size. A data URI may contain hundreds of kilobytes of base64, but vision
// models encode the image as visual tokens; counting the URI as ordinary text
// rejects valid local Artifact inputs before they reach the provider.
func estimatePromptInput(messages []ChatMessage) (textChars, tokens int) {
	var imageTokens int
	for _, msg := range messages {
		textChars += len([]rune(msg.Role))
		textChars += len([]rune(msg.Content))
		for _, part := range msg.Parts {
			textChars += len([]rune(part.Type))
			textChars += len([]rune(part.Text))
			if part.ImageURL != nil {
				imageTokens += estimateImageTokens(part.ImageURL.Detail)
			}
			if part.InputAudio != nil {
				textChars += len([]rune(part.InputAudio.Data))
				textChars += len([]rune(part.InputAudio.Format))
			}
		}
	}
	return textChars, estimateTokensFromChars(textChars) + imageTokens
}

// These are deliberately provider-neutral and conservative. Provider usage,
// when returned, remains authoritative for accounting. The estimate exists to
// prevent obvious context overflow without coupling Harness to one vendor's
// image tiling formula.
func estimateImageTokens(detail string) int {
	switch detail {
	case "low":
		return 256
	case "high":
		return 2048
	default: // empty and auto
		return 1024
	}
}
