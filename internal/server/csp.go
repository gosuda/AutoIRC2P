package server

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var inlineScripts = regexp.MustCompile(`(?s)<script\b[^>]*>(.*?)</script>`)

func scriptPolicy(webDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(webDir, "index.html"))
	if errors.Is(err, os.ErrNotExist) {
		return "'self'", nil
	}
	if err != nil {
		return "", err
	}
	var policy strings.Builder
	policy.WriteString("'self'")
	for _, script := range inlineScripts.FindAllSubmatch(data, -1) {
		if len(script[1]) == 0 {
			continue
		}
		digest := sha256.Sum256(script[1])
		fmt.Fprintf(&policy, " 'sha256-%s'", base64.StdEncoding.EncodeToString(digest[:]))
	}
	return policy.String(), nil
}
