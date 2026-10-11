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
	sub := strings.TrimSpace(r.Header.Get(a.SubjectHeader))
	if sub == "" {
		sub = a.DefaultSubject
	}
	if sub == "" {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Issuer: a.Issuer, Subject: sub, Email: strings.TrimSpace(r.Header.Get(a.EmailHeader))}, nil
}
