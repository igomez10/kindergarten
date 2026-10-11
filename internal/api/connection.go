package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/repository"
)

// Secrets reads what clients need to connect. Implemented over the
// Kubernetes API (internal/kubernetes.SecretReader).
type Secrets interface {
	AppCredentials(ctx context.Context, namespace, cluster string) (user, password string, err error)
	CACert(ctx context.Context, namespace, cluster string) ([]byte, error)
}

// connectable loads an instance the caller may connect to: theirs, wanted
// running, and with a known endpoint.
func (s *Server) connectable(w http.ResponseWriter, r *http.Request) (domain.Instance, bool) {
	id, ok := pathUUID(w, r)
	if !ok {
		return domain.Instance{}, false
	}
	inst, err := s.repo.GetInstance(r.Context(), id, principalFrom(r).orgIDs())
	if errors.Is(err, repository.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return inst, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return inst, false
	}
	if inst.DesiredState != domain.DesiredRunning || inst.EndpointHost == nil || inst.EndpointPort == nil {
		writeError(w, http.StatusConflict, "not_ready", "the instance has no endpoint yet")
		return inst, false
	}
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "connection details are not available in this deployment")
		return inst, false
	}
	return inst, true
}

func sslMode(inst domain.Instance) string {
	if inst.EndpointAddress != nil {
		return "verify-full" // external endpoint with the hostname in the certificate
	}
	return "require"
}

func caFileName(inst domain.Instance) string { return inst.Name + "-ca.crt" }

// libpqParams is the keyword/value connection string, without a password.
func libpqParams(inst domain.Instance) string {
	s := fmt.Sprintf("host=%s port=%d dbname=app user=app sslmode=%s", *inst.EndpointHost, *inst.EndpointPort, sslMode(inst))
	if inst.EndpointAddress != nil {
		s += fmt.Sprintf(" hostaddr=%s sslrootcert=%s", *inst.EndpointAddress, caFileName(inst))
	}
	return s
}

func connURI(inst domain.Instance, user, password string) string {
	u := url.URL{Scheme: "postgresql", Host: fmt.Sprintf("%s:%d", *inst.EndpointHost, *inst.EndpointPort), Path: "/app"}
	if password != "" {
		u.User = url.UserPassword(user, password)
	} else {
		u.User = url.User(user)
	}
	q := url.Values{"sslmode": {sslMode(inst)}}
	if inst.EndpointAddress != nil {
		q.Set("hostaddr", *inst.EndpointAddress)
		q.Set("sslrootcert", caFileName(inst))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// connection returns everything except the password.
func (s *Server) connection(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.connectable(w, r)
	if !ok {
		return
	}
	ca, err := s.secrets.CACert(r.Context(), inst.K8sNamespace, inst.K8sName)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host":         *inst.EndpointHost,
		"address":      inst.EndpointAddress,
		"port":         *inst.EndpointPort,
		"database":     "app",
		"user":         "app",
		"sslmode":      sslMode(inst),
		"ca_cert":      string(ca),
		"ca_file_name": caFileName(inst),
		"psql":         fmt.Sprintf("psql \"%s\"", libpqParams(inst)),
		"uri":          connURI(inst, "app", ""),
	})
}

// credentials returns the application password. Explicitly requested only,
// never cached, never logged.
func (s *Server) credentials(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.connectable(w, r)
	if !ok {
		return
	}
	user, password, err := s.secrets.AppCredentials(r.Context(), inst.K8sNamespace, inst.K8sName)
	if err != nil {
		s.internalError(w, r, errors.New("read application credentials failed"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"user":     user,
		"password": password,
		"uri":      connURI(inst, user, password),
	})
}
