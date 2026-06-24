package service

import (
	"encoding/base64"
	"strings"
)

const aggregateLocationSource = "all"

func encodeSearchLocation(source, id, fullPath string) string {
	if source == "" {
		source = aggregateLocationSource
	}
	return source + "|" + b64(id) + "|" + b64(fullPath)
}

// ParseSearchLocation decodes a value emitted by ListSearchLocations.
func ParseSearchLocation(v string) (SearchLocationFilter, bool) {
	parts := strings.Split(v, "|")
	if len(parts) != 3 {
		return SearchLocationFilter{}, false
	}
	id, ok := unb64(parts[1])
	if !ok {
		return SearchLocationFilter{}, false
	}
	fullPath, ok := unb64(parts[2])
	if !ok || strings.TrimSpace(fullPath) == "" {
		return SearchLocationFilter{}, false
	}
	source := parts[0]
	if source == aggregateLocationSource {
		source = ""
	}
	return SearchLocationFilter{Source: source, ID: id, FullPath: fullPath}, true
}

func b64(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func unb64(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	return string(b), true
}
