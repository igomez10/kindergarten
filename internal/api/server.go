// Package api is the platform's REST API (plan §2.4). It records desired state
// and serves cached observed state; it never talks to Kubernetes on the
// request path.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/igomez10/kindergarten/internal/auth"
	"github.com/igomez10/kindergarten/internal/domain"
	"github.com/igomez10/kindergarten/internal/repository"
)

type Config struct {
	// NamespaceOverride, when set, puts every Cluster in this one namespace
	// instead of the organization's own. Used until tenant namespaces are
	// bootstrapped automatically.
	NamespaceOverride string
	// WebDir, when set, serves the dashboard's static export from it.
	WebDir string
}

type Server struct {
	repo    *repository.Repo
	auth    auth.Authenticator
	cfg     Config
	logger  *slog.Logger
	secrets Secrets // nil: connection details unavailable
}

// WithSecrets enables the connection and credentials endpoints.
func (s *Server) WithSecrets(sec Secrets) *Server {
	s.secrets = sec
	return s
}

func New(repo *repository.Repo, a auth.Authenticator, cfg Config, logger *slog.Logger) *Server {
	return &Server{repo: repo, auth: a, cfg: cfg, logger: logger}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, s.logRequests)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/readyz", s.readyz)

	r.Route("/v1", func(r chi.Router) {
		r.Use(csrfGuard, s.authenticate)
		r.Get("/me", s.me)
		r.Get("/plans", s.listPlans)
		r.Post("/instances", s.createInstance)
		r.Get("/instances", s.listInstances)
		r.Get("/instances/{id}", s.getInstance)
		r.Delete("/instances/{id}", s.deleteInstance)
		r.Get("/instances/{id}/connection", s.connection)
		r.Get("/instances/{id}/credentials", s.credentials)
		r.Get("/instances/{id}/operations", s.listOperations)
		r.Get("/operations/{id}", s.getOperation)
	})
	if s.cfg.WebDir != "" {
		r.NotFound(webHandler(s.cfg.WebDir).ServeHTTP)
	}
	return r
}

// HealthRoutes serves only /healthz and /readyz.
func (s *Server) HealthRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/readyz", s.readyz)
	return r
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(), "request_id", middleware.GetReqID(r.Context()))
	})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.repo.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "metadata database unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// principal is the authenticated caller and the organizations they belong to.
type principal struct {
	identity    auth.Identity
	user        domain.User
	memberships []repository.Membership
}

func (p principal) orgIDs() []uuid.UUID {
	ids := make([]uuid.UUID, len(p.memberships))
	for i, m := range p.memberships {
		ids[i] = m.Organization.ID
	}
	return ids
}

type ctxKey struct{}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.auth.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		user, ms, err := s.repo.EnsureUser(r.Context(), id.Issuer, id.Subject, id.Email)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, principal{identity: id, user: user, memberships: ms})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func principalFrom(r *http.Request) principal { return r.Context().Value(ctxKey{}).(principal) }

// selectedOrg resolves the organization a write acts on: X-Organization-ID if
// given (and the caller is a member), otherwise the caller's personal one.
func (s *Server) selectedOrg(w http.ResponseWriter, r *http.Request) (domain.Organization, bool) {
	p := principalFrom(r)
	want := r.Header.Get("X-Organization-ID")
	if want == "" {
		if len(p.memberships) == 0 {
			writeError(w, http.StatusForbidden, "no_organization", "you are not a member of any organization")
			return domain.Organization{}, false
		}
		return p.memberships[0].Organization, true
	}
	for _, m := range p.memberships {
		if m.Organization.ID.String() == want {
			return m.Organization, true
		}
	}
	writeError(w, http.StatusForbidden, "forbidden", "you are not a member of that organization")
	return domain.Organization{}, false
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	type orgJSON struct {
		ID    string           `json:"id"`
		Name  string           `json:"name"`
		Role  string           `json:"role"`
		Quota map[string]int   `json:"quota"`
		Usage repository.Usage `json:"usage"`
	}
	orgs := []orgJSON{}
	for _, m := range p.memberships {
		u, err := s.repo.Usage(r.Context(), m.Organization.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		o := m.Organization
		orgs = append(orgs, orgJSON{ID: o.ID.String(), Name: o.Name, Role: m.Role, Usage: u, Quota: map[string]int{
			"instances": o.MaxInstances, "cpu_millis": o.MaxCPUMillis, "memory_mib": o.MaxMemoryMiB, "storage_gib": o.MaxStorageGiB,
		}})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":          map[string]any{"id": p.user.ID, "email": p.user.Email, "subject": p.user.Subject},
		"organizations": orgs,
	})
}

func (s *Server) listPlans(w http.ResponseWriter, _ *http.Request) {
	versions := make([]string, 0, len(domain.PostgresImages))
	for v := range domain.PostgresImages {
		versions = append(versions, v)
	}
	sortStrings(versions)
	writeJSON(w, http.StatusOK, map[string]any{
		"plans":                    domain.Plans(),
		"postgres_versions":        versions,
		"default_postgres_version": domain.DefaultPostgresVersion,
	})
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request) {
	org, ok := s.selectedOrg(w, r)
	if !ok {
		return
	}
	var req domain.CreateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	req, plan, err := req.Normalize()
	var verr *domain.ValidationError
	if errors.As(err, &verr) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Code: "invalid_request", Message: verr.Message, Field: verr.Field}})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key is longer than 200 characters")
		return
	}
	ns := org.K8sNamespace
	if s.cfg.NamespaceOverride != "" {
		ns = s.cfg.NamespaceOverride
	}
	res, err := s.repo.CreateInstance(r.Context(), repository.CreateParams{
		OrganizationID: org.ID, IdempotencyKey: key, Request: req, Plan: plan, K8sNamespace: ns,
	})
	var qerr *repository.QuotaError
	switch {
	case errors.Is(err, repository.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
		return
	case errors.Is(err, repository.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", err.Error())
		return
	case errors.As(err, &qerr):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: errorDetail{Code: "quota_exceeded", Message: qerr.Error(), Field: qerr.Resource}})
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Location", "/v1/instances/"+res.Instance.ID.String())
	writeJSON(w, http.StatusAccepted, map[string]any{
		"instance":  map[string]any{"id": res.Instance.ID, "name": res.Instance.Name, "status": res.Instance.ObservedStatus},
		"operation": map[string]any{"id": res.Operation.ID, "status": res.Operation.Status},
	})
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.repo.ListInstances(r.Context(), principalFrom(r).orgIDs())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]instanceJSON, len(insts))
	for i, inst := range insts {
		out[i] = toInstanceJSON(inst)
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
}

// pathUUID parses {id}; an unparseable ID is a 404, like any unknown one.
func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) getInstance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	inst, err := s.repo.GetInstance(r.Context(), id, principalFrom(r).orgIDs())
	if errors.Is(err, repository.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toInstanceJSON(inst))
}

func (s *Server) deleteInstance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	inst, op, err := s.repo.RequestDelete(r.Context(), id, principalFrom(r).orgIDs())
	if errors.Is(err, repository.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "instance not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"instance":  map[string]any{"id": inst.ID, "name": inst.Name, "status": inst.ObservedStatus},
		"operation": map[string]any{"id": op.ID, "status": op.Status},
	})
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if _, err := s.repo.GetInstance(r.Context(), id, principalFrom(r).orgIDs()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "instance not found")
		} else {
			s.internalError(w, r, err)
		}
		return
	}
	ops, err := s.repo.ListOperations(r.Context(), id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]operationJSON, len(ops))
	for i, op := range ops {
		out[i] = toOperationJSON(op)
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": out})
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	op, err := s.repo.GetOperation(r.Context(), id, principalFrom(r).orgIDs())
	if errors.Is(err, repository.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toOperationJSON(op))
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("internal error", "path", r.URL.Path, "err", err, "request_id", middleware.GetReqID(r.Context()))
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}
