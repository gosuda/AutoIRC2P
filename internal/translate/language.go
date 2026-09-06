package translate

import (
	"strings"
	"sync"
	"unicode"

	"github.com/pemistahl/lingua-go"
)

var languageNames = map[string]string{
	"en": "English", "es": "Spanish", "zh": "Chinese", "ko": "Korean",
	"ja": "Japanese", "de": "German", "ru": "Russian", "fr": "French",
	"nl": "Dutch", "it": "Italian", "id": "Indonesian", "pt": "Portuguese",
	"sv": "Swedish", "cs": "Czech",
}

// Language set and builder follow gosuda/website/llm.go.
var detector = sync.OnceValue(func() lingua.LanguageDetector {
	return lingua.NewLanguageDetectorBuilder().FromLanguages(
		lingua.English, lingua.Spanish, lingua.Chinese, lingua.Korean,
		lingua.Japanese, lingua.German, lingua.Russian, lingua.French,
		lingua.Dutch, lingua.Italian, lingua.Indonesian, lingua.Portuguese,
		lingua.Swedish, lingua.Czech,
	).Build()
})

// Detect returns und when there is no linguistic content or no confident match.
func Detect(text string) string {
	text = protectedText.ReplaceAllString(text, " ")
	letters := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 2 {
		return "und"
	}
	language, ok := detector().DetectLanguageOf(text)
	if !ok || detector().ComputeLanguageConfidence(text, language) < 0.6 {
		return "und"
	}
	return mapDetectedLanguage(language)
}

// Mapping follows gosuda/website/utils.go, with Czech and unknown made explicit.
func mapDetectedLanguage(language lingua.Language) string {
	switch language {
	case lingua.English:
		return "en"
	case lingua.Spanish:
		return "es"
	case lingua.Chinese:
		return "zh"
	case lingua.Korean:
		return "ko"
	case lingua.Japanese:
		return "ja"
	case lingua.German:
		return "de"
	case lingua.Russian:
		return "ru"
	case lingua.French:
		return "fr"
	case lingua.Dutch:
		return "nl"
	case lingua.Italian:
		return "it"
	case lingua.Indonesian:
		return "id"
	case lingua.Portuguese:
		return "pt"
	case lingua.Swedish:
		return "sv"
	case lingua.Czech:
		return "cs"
	default:
		return "und"
	}
}

// RoomLanguage expects oldest-to-newest detections. Unknowns are not votes.
func RoomLanguage(languages []string) string {
	if len(languages) > 100 {
		languages = languages[len(languages)-100:]
	}
	useful, russian, german := 0, 0, 0
	for _, language := range languages {
		language = strings.ToLower(language)
		if _, ok := languageNames[language]; !ok {
			continue
		}
		useful++
		switch language {
		case "ru":
			russian++
		case "de":
			german++
		}
	}
	if useful < 20 {
		return "en"
	}
	if russian*100 >= useful*60 {
		return "ru"
	}
	if german*100 >= useful*60 {
		return "de"
	}
	return "en"
}
