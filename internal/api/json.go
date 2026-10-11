package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/igomez10/kindergarten/internal/domain"
)

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

func sortStrings(s []string) { sort.Strings(s) }

type endpointJSON struct {
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Address *string `json:"address"`
}

type instanceJSON struct {
	ID              string        `json:"id"`
	OrganizationID  string        `json:"organization_id"`
	Name            string        `json:"name"`
	Type            string        `json:"type"`
	Plan            string        `json:"plan"`
	PostgresVersion string        `json:"postgres_version"`
	CPUMillis       int           `json:"cpu_millis"`
	MemoryMiB       int           `json:"memory_mib"`
	StorageGiB      int           `json:"storage_gib"`
	DesiredState    string        `json:"desired_state"`
	Status          string        `json:"status"`
	Endpoint        *endpointJSON `json:"endpoint"`
	StatusMessage   *string       `json:"status_message"`
	Error           *string       `json:"error"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

func toInstanceJSON(i domain.Instance) instanceJSON {
	out := instanceJSON{
		ID: i.ID.String(), OrganizationID: i.OrganizationID.String(), Name: i.Name, Type: i.ServiceType,
		Plan: i.Spec.Plan, PostgresVersion: i.Spec.PostgresVersion,
		CPUMillis: i.ReservedCPUMillis, MemoryMiB: i.ReservedMemoryMiB, StorageGiB: i.ReservedStorageGiB,
		DesiredState: i.DesiredState, Status: i.ObservedStatus, StatusMessage: i.StatusMessage, Error: i.LastError,
		CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
	}
	if i.EndpointHost != nil && i.EndpointPort != nil {
		out.Endpoint = &endpointJSON{Host: *i.EndpointHost, Port: *i.EndpointPort, Address: i.EndpointAddress}
	}
	return out
}

type operationJSON struct {
	ID          string     `json:"id"`
	InstanceID  string     `json:"instance_id"`
	Action      string     `json:"action"`
	Status      string     `json:"status"`
	Error       *string    `json:"error"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

func toOperationJSON(o domain.Operation) operationJSON {
	return operationJSON{ID: o.ID.String(), InstanceID: o.InstanceID.String(), Action: o.Action, Status: o.Status,
		Error: o.ErrorMessage, CreatedAt: o.CreatedAt, CompletedAt: o.CompletedAt}
}
