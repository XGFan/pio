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
	"Proxy-Connection", // non-standard, but browsers and curl still send it
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// StripHopByHop removes the canonical hop-by-hop set plus any headers
// named in the inbound Connection token list (RFC 7230 §6.1: a sender
// may extend the hop-by-hop set by listing additional header names there).
func StripHopByHop(h http.Header) {
	// Read the Connection tokens before the loop below deletes Connection.
	for _, conn := range h.Values("Connection") {
		for tok := range strings.SplitSeq(conn, ",") {
			if name := strings.TrimSpace(tok); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}
