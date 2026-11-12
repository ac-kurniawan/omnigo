package commandcode

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"sync"
	"unicode"
)

const CommandCodeCLIVersion = "1.88.5"

var (
	projectSlugOnce sync.Once
	projectSlug     string
)

func buildCommandCodeHeaders(key, sessionID string, accept string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+key)
	if accept != "" {
		h.Set("Accept", accept)
	}
	h.Set("User-Agent", "cli")
	h.Set("x-command-code-version", CommandCodeCLIVersion)
	h.Set("x-cli-environment", "production")
	h.Set("x-cli-surface", "cli")
	h.Set("x-taste-learning", "false")
	h.Set("x-project-slug", currentProjectSlug())
	if sessionID != "" {
		h.Set("x-session-id", sessionID)
	}
	return h
}

func newThreadID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "thread-00000000-0000-0000-0000-000000000000"
	}
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" + hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

func currentProjectSlug() string {
	projectSlugOnce.Do(func() {
		cwd, err := os.Getwd()
		if err != nil {
			projectSlug = "omnigo"
			return
		}
		parts := strings.FieldsFunc(cwd, func(r rune) bool { return r == '/' || r == '\\' })
		if len(parts) == 0 {
			projectSlug = "omnigo"
			return
		}
		projectSlug = slugify(parts[len(parts)-1])
		if projectSlug == "" {
			projectSlug = "omnigo"
		}
	})
	return projectSlug
}

func slugify(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	prevHyphen := false
	for i, r := range s {
		// decamelize: insert hyphen before upper if preceded by lower/digit
		if unicode.IsUpper(r) && i > 0 {
			prev := []rune(s[:i])
			if len(prev) > 0 {
				p := prev[len(prev)-1]
				if unicode.IsLower(p) || unicode.IsDigit(p) {
					if !prevHyphen {
						b.WriteByte('-')
					}
				}
			}
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			for _, c := range transliterate(r) {
				b.WriteRune(unicode.ToLower(c))
			}
			prevHyphen = false
			continue
		}
		if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}

func transliterate(r rune) string {
	switch r {
	case 'ä', 'Ä', 'æ', 'Æ':
		return "ae"
	case 'ö', 'Ö', 'ø', 'Ø':
		return "oe"
	case 'ü', 'Ü':
		return "ue"
	case 'ß':
		return "ss"
	case 'å', 'Å':
		return "aa"
	default:
		return string(r)
	}
}
