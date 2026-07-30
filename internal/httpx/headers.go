package httpx

import (
	"net/http"
	"strings"
)

func CloneEndToEndHeaders(source http.Header) http.Header {
	target := make(http.Header, len(source))
	CopyEndToEndHeaders(target, source)
	return target
}

func CopyEndToEndHeaders(target, source http.Header) {
	for key, values := range source {
		if IsHopByHopHeader(key) || strings.EqualFold(key, "Host") {
			continue
		}
		target[key] = append([]string(nil), values...)
	}
}

func IsHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
