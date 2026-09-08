package translate

import (
	"strings"
)

var languageNames = map[string]string{
	"en": "English", "es": "Spanish", "zh": "Chinese", "ko": "Korean",
	"ja": "Japanese", "de": "German", "ru": "Russian", "fr": "French",
	"nl": "Dutch", "it": "Italian", "id": "Indonesian", "pt": "Portuguese",
	"sv": "Swedish", "cs": "Czech",
}

func RoomTargetLanguage(room string) string {
	suffix := room
	if len(room) > 3 {
		suffix = room[len(room)-3:]
	}
	switch {
	case strings.EqualFold(room, "#ru") || strings.EqualFold(suffix, "-ru"):
		return "ru"
	case strings.EqualFold(room, "#de") || strings.EqualFold(suffix, "-de"):
		return "de"
	case strings.EqualFold(room, "#ko"):
		return "ko"
	default:
		return "en"
	}
}
