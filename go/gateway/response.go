package gateway

import (
	"io"
	"net/http"
	"strings"
)

type isolatedResponseWriter struct {
	http.ResponseWriter
}

func (w *isolatedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *isolatedResponseWriter) WriteHeader(code int) {
	isolateResponseHeaders(w.Header())
	w.ResponseWriter.WriteHeader(code)
}

func isolateResponse(response *http.Response) error {
	isolateResponseHeaders(response.Header)
	isolateResponseHeaders(response.Trailer)
	if response.StatusCode != http.StatusSwitchingProtocols {
		response.Body = &isolatedResponseBody{ReadCloser: response.Body, response: response}
	}
	return nil
}

func isolateResponseHeaders(headers http.Header) {
	cookies := headers.Values("Set-Cookie")
	headers.Del("Set-Cookie")
	for _, cookie := range cookies {
		hasDomain := false
		for _, attribute := range strings.Split(cookie, ";")[1:] {
			name, _, _ := strings.Cut(attribute, "=")
			if strings.EqualFold(strings.TrimSpace(name), "domain") {
				hasDomain = true
				break
			}
		}
		if !hasDomain {
			headers.Add("Set-Cookie", cookie)
		}
	}
	headers.Del("Set-Cookie2")
	// Cookie clearing is registrable-domain-wide, not confined to this origin.
	headers.Del("Clear-Site-Data")
}

type isolatedResponseBody struct {
	io.ReadCloser
	response *http.Response
}

func (b *isolatedResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		isolateResponseHeaders(b.response.Trailer)
	}
	return n, err
}
