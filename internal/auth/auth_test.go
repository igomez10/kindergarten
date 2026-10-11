package auth

import (
	"errors"
	"net/http"
	"testing"
)

func TestProxyAuthenticate(t *testing.T) {
	a := Proxy("https://issuer.example")
	cases := []struct {
		name    string
		headers map[string][]string
		want    string
		err     error
	}{
		{"subject and email", map[string][]string{"X-Forwarded-User": {"123"}, "X-Forwarded-Email": {"a@example.com"}}, "123", nil},
		{"no subject", map[string][]string{"X-Forwarded-Email": {"a@example.com"}}, "", ErrUnauthenticated},
		{"forged subject before the proxy's", map[string][]string{"X-Forwarded-User": {"victim", "123"}}, "", ErrUnauthenticated},
		{"comma-joined subject", map[string][]string{"X-Forwarded-User": {"victim,123"}}, "", ErrUnauthenticated},
		{"repeated email", map[string][]string{"X-Forwarded-User": {"123"}, "X-Forwarded-Email": {"v@example.com", "a@example.com"}}, "", ErrUnauthenticated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodGet, "/", nil)
			for k, vs := range c.headers {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			id, err := a.Authenticate(r)
			if !errors.Is(err, c.err) || id.Subject != c.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", id.Subject, err, c.want, c.err)
			}
			if err == nil && id.Issuer != "https://issuer.example" {
				t.Fatalf("issuer %q", id.Issuer)
			}
		})
	}
}

func TestDevDefaultSubject(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	id, err := DevAs("alice").Authenticate(r)
	if err != nil || id.Subject != "alice" || id.Issuer != "dev" {
		t.Fatalf("got (%+v, %v)", id, err)
	}
}
