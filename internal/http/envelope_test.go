package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tera/internal/repository"
)

// The API contract has three properties the rest of this file pins, all of
// which used to hold only by accident: every response is the standard envelope
// (including the health probes and the routing errors), a journal mutation
// answers with the same shape as the journal list, and pagination metadata
// describes the unpaged total.

func envelopeTestLogin(t *testing.T, handler http.Handler) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		bytes.NewBufferString(`{"email":"demo@tera.co.id","password":"Demo1234"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)

	var envelope struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Token == "" {
		t.Fatalf("login did not return a token: %s", recorder.Body.String())
	}
	return envelope.Data.Token
}

func envelopeTestJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not JSON: %s", recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	return envelope
}

// /healthz answers in the envelope. The demo-reset workflow and the hosting
// probes grep the raw body for the literal "status":"ok", which the envelope
// still contains - nested under data rather than at the top level.
func TestHealthzUsesEnvelopeAndKeepsStatusOKLiteral(t *testing.T) {
	handler := NewServer(repository.NewMemoryStore()).Router()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("body no longer contains \"status\":\"ok\": %s", recorder.Body.String())
	}
	data, ok := envelopeTestJSON(t, recorder)["data"].(map[string]any)
	if !ok || data["status"] != "ok" {
		t.Fatalf("healthz is not enveloped: %s", recorder.Body.String())
	}
}

// An unmatched API path and a wrong method both answer in the envelope. Go's
// mux would otherwise emit plain text, which no client can parse.
func TestUnmatchedRouteAndWrongMethodAnswerInEnvelope(t *testing.T) {
	handler := NewServer(repository.NewMemoryStore()).Router()
	token := envelopeTestLogin(t, handler)

	notFound := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(notFound, request)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", notFound.Code)
	}
	if errors, _ := envelopeTestJSON(t, notFound)["errors"].([]any); len(errors) != 1 {
		t.Fatalf("404 body is not enveloped: %s", notFound.Body.String())
	}

	notAllowed := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodDelete, "/api/v1/accounts", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(notAllowed, request)
	if notAllowed.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d, want 405", notAllowed.Code)
	}
	if allow := notAllowed.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("Allow = %q, want \"GET, POST\"", allow)
	}
	if !strings.Contains(notAllowed.Body.String(), `"METHOD_NOT_ALLOWED"`) {
		t.Fatalf("405 body is not enveloped: %s", notAllowed.Body.String())
	}
}

// Create, update and post answer with the list projection, so the frontend can
// splice the response into its rows. domain.JournalEntry has no total and no
// source, so returning it directly rendered every saved journal as Rp 0.
func TestJournalMutationsReturnTheListShape(t *testing.T) {
	handler := NewServer(repository.NewMemoryStore()).Router()
	token := envelopeTestLogin(t, handler)
	payload := `{"date":"2026-03-01","memo":"shape check",
		"lines":[{"accountCode":"1100","debit":250000},{"accountCode":"4100","credit":250000}]}`

	post := func(method, path string) map[string]any {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, bytes.NewBufferString(payload))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
			t.Fatalf("%s %s = %d %s", method, path, recorder.Code, recorder.Body.String())
		}
		data, ok := envelopeTestJSON(t, recorder)["data"].(map[string]any)
		if !ok {
			t.Fatalf("%s %s returned no object: %s", method, path, recorder.Body.String())
		}
		return data
	}

	created := post(http.MethodPost, "/api/v1/journal-entries")
	if created["total"] != float64(250000) || created["source"] != "Manual" {
		t.Fatalf("create response lacks total/source: %#v", created)
	}
	id := fmt.Sprint(int(created["id"].(float64)))

	if updated := post(http.MethodPut, "/api/v1/journal-entries/"+id); updated["total"] != float64(250000) {
		t.Fatalf("update response lacks total: %#v", updated)
	}
	posted := post(http.MethodPost, "/api/v1/journal-entries/"+id+"/post")
	if posted["total"] != float64(250000) || posted["status"] != "Posted" {
		t.Fatalf("post response lacks total or status: %#v", posted)
	}
}

// Every list endpoint returns meta describing the unpaged total, and perPage is
// clamped to the documented range.
func TestListEndpointsReturnPageCountMetadata(t *testing.T) {
	handler := NewServer(repository.NewMemoryStore()).Router()
	token := envelopeTestLogin(t, handler)

	read := func(query string) (map[string]any, map[string]any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/accounts"+query, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("accounts%s = %d %s", query, recorder.Code, recorder.Body.String())
		}
		envelope := envelopeTestJSON(t, recorder)
		meta, _ := envelope["meta"].(map[string]any)
		return meta, envelope
	}

	meta, envelope := read("")
	total, perPage := meta["total"].(float64), meta["perPage"].(float64)
	if perPage != 50 {
		t.Fatalf("default perPage = %v, want 50", perPage)
	}
	if rows, _ := envelope["data"].([]any); float64(len(rows)) > perPage {
		t.Fatalf("returned %d rows for perPage %v", len(rows), perPage)
	}
	if want := int64(math.Ceil(total / perPage)); meta["totalPages"] != float64(want) {
		t.Fatalf("totalPages = %v, want %v (total=%v perPage=%v)", meta["totalPages"], want, total, perPage)
	}

	over := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/accounts?perPage=201", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(over, request)
	if over.Code != http.StatusUnprocessableEntity {
		t.Fatalf("perPage=201 = %d, want 422", over.Code)
	}
}
