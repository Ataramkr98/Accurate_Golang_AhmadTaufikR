package httpapi

import (
	"encoding/json"
	"net/http"

	"tera/internal/domain"
)

// Envelope is the consistent API response shape (PRD §10.10). Every endpoint
// answers with it - including the health probes and the 404/405 handlers - so a
// client never has to branch on "did this route return JSON or plain text".
type Envelope struct {
	Data   any        `json:"data,omitempty"`
	Meta   *Meta      `json:"meta,omitempty"`
	Errors []APIError `json:"errors,omitempty"`
}

// Meta carries pagination and similar response metadata. Total is the number of
// rows matching the filter across all pages, so a client can size its pager
// without walking every page.
type Meta struct {
	Page       int   `json:"page"`
	PerPage    int   `json:"perPage"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"totalPages"`
}

// APIError is a single machine-readable error. Fields is present only for
// validation failures, where it maps a request field name to the reason it was
// rejected; the frontend highlights those inputs (accurate-web/src/api/client.js
// reads errors[0].fields).
type APIError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// NewMeta builds pagination metadata, deriving the page count from the unpaged
// total. An empty result set has zero pages rather than one empty page, so a
// client can tell "no records" from "page 1 of 1 that happens to be empty".
func NewMeta(page, perPage int, total int64) Meta {
	meta := Meta{Page: page, PerPage: perPage, Total: total}
	if perPage > 0 && total > 0 {
		meta.TotalPages = int((total + int64(perPage) - 1) / int64(perPage))
	}
	return meta
}

// OK writes a successful response with data.
func OK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, Envelope{Data: data})
}

// OKWithMeta writes a successful paginated response.
func OKWithMeta(w http.ResponseWriter, data any, meta Meta) {
	writeJSON(w, http.StatusOK, Envelope{Data: data, Meta: &meta})
}

// Created writes a 201 response with data.
func Created(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusCreated, Envelope{Data: data})
}

// Accepted writes a 202 response with data.
func Accepted(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusAccepted, Envelope{Data: data})
}

// Error writes a domain-aware error response. A domain error's field map is
// passed through so a 422 can say which input was wrong.
func Error(w http.ResponseWriter, err error) {
	if dErr, ok := err.(*domain.Error); ok {
		writeJSON(w, dErr.Status, Envelope{
			Errors: []APIError{{
				Code:    dErr.Code,
				Message: dErr.Message,
				Fields:  dErr.Fields,
			}},
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, Envelope{
		Errors: []APIError{{Code: domain.CodeInternal, Message: "internal server error"}},
	})
}

// ValidationError writes a 422 with per-field reasons. It is the shorthand for
// the common case of a value that is well-formed but out of range.
func ValidationError(w http.ResponseWriter, message string, fields map[string]string) {
	Error(w, domain.NewValidationError(message, fields))
}

// NoContent writes a 204 with no body.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
