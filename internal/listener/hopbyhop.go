package listener

import (
	"net/http"
	"strings"
)

// hopByHopHeaders is the fixed list of headers that RFC 7230 §6.1 names
// as connection-scoped. They must never be forwarded across a proxy hop.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// StripHopByHop removes the canonical hop-by-hop set plus any headers
// named in the inbound Connection token list (RFC 7230 §6.1: a sender
// may extend the hop-by-hop set by listing additional header names there).
func StripHopByHop(h http.Header) {
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
	if conn := h.Get("Connection"); conn != "" {
		for tok := range strings.SplitSeq(conn, ",") {
			if name := strings.TrimSpace(tok); name != "" {
				h.Del(name)
			}
		}
	}
}
