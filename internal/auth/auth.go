// Package auth turns an incoming request into a verified identity.
//
// The API never sees passwords or tokens. In production it sits behind an
// oauth2-proxy sidecar that completes the ZITADEL login (OIDC code flow with
// PKCE) and forwards the verified subject in X-Forwarded-User, overwriting
// anything the client sent. That header is only trustworthy because the API
// listens on the pod's loopback, where the proxy is the only client.
package auth

import (
	"errors"
	"net/http"
	"strings"
)

var ErrUnauthenticated = errors.New("unauthenticated")

// Identity is who made the request: an (issuer, subject) pair, never an email.
type Identity struct {
	Issuer  string
	Subject string
	Email   string
}

// Authenticator extracts an Identity from a request.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// HeaderAuthenticator reads identity from trusted request headers.
type HeaderAuthenticator struct {
	Issuer        string
	SubjectHeader string
	EmailHeader   string
	// DefaultSubject is used when the header is absent. Dev mode only, so a
	// browser on localhost can use the dashboard without a login.
	DefaultSubject string
}

// Proxy trusts oauth2-proxy's headers. Only for a loopback listener behind it.
func Proxy(issuer string) HeaderAuthenticator {
	return HeaderAuthenticator{Issuer: issuer, SubjectHeader: "X-Forwarded-User", EmailHeader: "X-Forwarded-Email"}
}

// Dev trusts X-Dev-User. Local development and tests only: anyone who can
// reach the listener can be anyone. Identities live under their own issuer,
// so they can never collide with real ZITADEL users.
func Dev() HeaderAuthenticator {
	return HeaderAuthenticator{Issuer: "dev", SubjectHeader: "X-Dev-User", EmailHeader: "X-Dev-Email"}
}

// DevAs is Dev with a default identity for requests that name none.
func DevAs(subject string) HeaderAuthenticator {
	a := Dev()
	a.DefaultSubject = subject
	return a
}

func (a HeaderAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	sub, err := single(r.Header, a.SubjectHeader)
	if err != nil {
		return Identity{}, err
	}
	email, err := single(r.Header, a.EmailHeader)
	if err != nil {
		return Identity{}, err
	}
	if sub == "" {
		sub = a.DefaultSubject
	}
	if sub == "" {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Issuer: a.Issuer, Subject: sub, Email: email}, nil
}

// single returns a header that must appear at most once. A repeated or
// comma-joined identity header means a client-sent copy reached the API next
// to the proxy's (oauth2-proxy appends rather than replaces when told not to
// strip), and either copy could be the forged one, so the request is refused.
func single(h http.Header, name string) (string, error) {
	values := h.Values(name)
	if len(values) > 1 || (len(values) == 1 && strings.Contains(values[0], ",")) {
		return "", ErrUnauthenticated
	}
	if len(values) == 0 {
		return "", nil
	}
	return strings.TrimSpace(values[0]), nil
}
