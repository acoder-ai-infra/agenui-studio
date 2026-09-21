package observability

import "strings"

type PreviewConfig struct {
	MaxBytes int
}

func PreviewText(value string, cfg PreviewConfig) (preview string, truncated bool) {
	limit := cfg.MaxBytes
	if limit <= 0 {
		limit = 1024
	}
	if len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}

func RedactBasic(value string) string {
	if value == "" {
		return value
	}
	redacted := value
	for _, marker := range []string{"api_key=", "token=", "password=", "secret="} {
		if idx := strings.Index(strings.ToLower(redacted), marker); idx >= 0 {
			end := strings.IndexAny(redacted[idx:], "& \n\t")
			if end < 0 {
				redacted = redacted[:idx+len(marker)] + "***"
			} else {
				redacted = redacted[:idx+len(marker)] + "***" + redacted[idx+end:]
			}
		}
	}
	return redacted
}
