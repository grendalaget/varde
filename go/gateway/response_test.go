package gateway

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestIsolateResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cookies []string
		want    []string
	}{
		{name: "empty"},
		{name: "host only", cookies: []string{"session=ok; Path=/; Secure; HttpOnly", "Domain=cookie-name; Path=/"}, want: []string{"session=ok; Path=/; Secure; HttpOnly", "Domain=cookie-name; Path=/"}},
		{name: "parent domain", cookies: []string{"session=bad; Domain=varde.games"}},
		{name: "leading dot", cookies: []string{"session=bad; domain=.varde.games"}},
		{name: "case and whitespace", cookies: []string{"session=bad; \tDoMaIn = .play.varde.games; Secure"}},
		{name: "own domain", cookies: []string{"session=bad; Domain=srv-example.play.varde.games"}},
		{name: "empty domain", cookies: []string{"session=bad; Domain="}},
		{name: "bare domain", cookies: []string{"session=bad; Domain"}},
		{name: "mixed", cookies: []string{"first=ok; Path=/", "session=bad; Domain=.varde.games", "last=ok; Expires=Wed, 09 Jun 2038 10:18:14 GMT"}, want: []string{"first=ok; Path=/", "last=ok; Expires=Wed, 09 Jun 2038 10:18:14 GMT"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			for _, cookie := range tc.cookies {
				headers.Add("Set-Cookie", cookie)
			}
			headers.Set("Clear-Site-Data", `"cookies", "storage"`)
			headers.Set("Set-Cookie2", "legacy=bad; Domain=.varde.games")
			headers.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			headers.Set("X-App-Header", "preserved")
			isolateResponseHeaders(headers)
			if got := headers.Values("Set-Cookie"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("isolateResponseHeaders(%q) cookies = %q, want %q", tc.cookies, got, tc.want)
			}
			if headers.Get("Clear-Site-Data") != "" || headers.Get("Set-Cookie2") != "" {
				t.Errorf("isolateResponseHeaders left site-scoped headers: %v", headers)
			}
			if headers.Get("Strict-Transport-Security") != "max-age=31536000; includeSubDomains" || headers.Get("X-App-Header") != "preserved" {
				t.Errorf("isolateResponseHeaders changed host-scoped headers: %v", headers)
			}
		})
	}
}

func TestIsolateResponseTrailers(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	response.Body = &trailerBody{Reader: strings.NewReader("body"), response: response}
	if err := isolateResponse(response); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if len(response.Trailer) != 1 || response.Trailer.Get("X-App-Trailer") != "preserved" {
		t.Errorf("isolateResponse trailers = %v, want only X-App-Trailer", response.Trailer)
	}
}

type trailerBody struct {
	*strings.Reader
	response *http.Response
}

func (b *trailerBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.response.Trailer = http.Header{"Set-Cookie": {"session=bad; Domain=.varde.games"}, "Clear-Site-Data": {`"cookies"`}, "X-App-Trailer": {"preserved"}}
	}
	return n, err
}

func (b *trailerBody) Close() error { return nil }
