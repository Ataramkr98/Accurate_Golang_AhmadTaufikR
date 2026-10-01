package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"tera/internal/repository"
)

func TestRegistrationOnboardingAndInvoiceFlow(t *testing.T) {
	t.Setenv("TERA_ENABLE_SIGNUP", "true")
	server := httptest.NewServer(NewServer(repository.NewMemoryStore()).Router())
	defer server.Close()

	registerResponse := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/register", "", map[string]any{
		"name": "Siti Aminah", "email": "SITI@example.com", "password": "Strong123", "companyName": "CV Siti",
	})
	if registerResponse.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", registerResponse.StatusCode, registerResponse.Body)
	}
	token, _ := registerResponse.Data["token"].(string)
	if token == "" {
		t.Fatal("register did not return a token")
	}

	accountsResponse := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/accounts", token, nil)
	if accountsResponse.StatusCode != http.StatusOK {
		t.Fatalf("accounts status = %d, body=%s", accountsResponse.StatusCode, accountsResponse.Body)
	}
	accounts, ok := accountsResponse.RawData.([]any)
	if !ok || len(accounts) < 10 {
		t.Fatalf("new tenant did not receive default accounts: %#v", accountsResponse.RawData)
	}

	customResponse := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/accounts", token, map[string]any{
		"code": "1199", "name": "Kas Proyek", "type": "Aset",
	})
	if customResponse.StatusCode != http.StatusCreated || number(customResponse.Data["parentId"]) == 0 {
		t.Fatalf("custom account was not attached to a header: status=%d body=%s", customResponse.StatusCode, customResponse.Body)
	}

	companyResponse := requestJSON(t, server.Client(), http.MethodPut, server.URL+"/api/v1/company", token, map[string]any{
		"name": "CV Siti", "npwp": "01.234.567.8-901.000", "sakMode": "psak", "isPkp": true, "businessType": "Jasa",
	})
	if companyResponse.StatusCode != http.StatusOK || companyResponse.Data["sakMode"] != "psak_umum" || companyResponse.Data["isPkp"] != true {
		t.Fatalf("company setup failed: status=%d body=%s", companyResponse.StatusCode, companyResponse.Body)
	}

	invoiceResponse := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/invoices", token, map[string]any{
		"customerName": "PT Pelanggan", "date": "2026-08-20", "dueDate": "2026-09-20",
		"lines": []map[string]any{{"name": "Jasa Audit", "qty": 1, "price": 1_000_000, "discountPercent": 0}},
	})
	if invoiceResponse.StatusCode != http.StatusCreated || number(invoiceResponse.Data["taxAmount"]) != 110_000 {
		t.Fatalf("invoice create failed: status=%d body=%s", invoiceResponse.StatusCode, invoiceResponse.Body)
	}
	invoiceID := uint(number(invoiceResponse.Data["id"]))
	issueResponse := requestJSON(t, server.Client(), http.MethodPost,
		server.URL+"/api/v1/invoices/"+fmt.Sprint(invoiceID)+"/issue", token, nil)
	if issueResponse.StatusCode != http.StatusOK || issueResponse.Data["status"] == "Draft" {
		t.Fatalf("invoice issue failed: status=%d body=%s", issueResponse.StatusCode, issueResponse.Body)
	}
	auditResponse := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/audit-logs", token, nil)
	auditLogs, ok := auditResponse.RawData.([]any)
	if auditResponse.StatusCode != http.StatusOK || !ok || len(auditLogs) < 2 {
		t.Fatalf("audit trail missing invoice/journal events: status=%d body=%s", auditResponse.StatusCode, auditResponse.Body)
	}

	duplicateResponse := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/register", "", map[string]any{
		"name": "Duplicate", "email": "siti@EXAMPLE.COM", "password": "Strong123", "companyName": "Duplicate Ltd",
	})
	if duplicateResponse.StatusCode != http.StatusConflict {
		t.Fatalf("case-insensitive duplicate status = %d, want 409", duplicateResponse.StatusCode)
	}

	logoutResponse := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/logout", token, nil)
	if logoutResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", logoutResponse.StatusCode)
	}
	meResponse := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/auth/me", token, nil)
	if meResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", meResponse.StatusCode)
	}
}

func TestHTTPValidationAndCORS(t *testing.T) {
	t.Setenv("TERA_ENABLE_PASSWORD_RESET", "true")
	handler := NewServer(repository.NewMemoryStore()).Router()

	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login status = %d, want 405", wrongMethod.Code)
	}

	unknownField := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"email":"a@b.com","password":"x","extra":true}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(unknownField, request)
	if unknownField.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field status = %d, want 422", unknownField.Code)
	}

	evilOrigin := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("Origin", "https://evil.example")
	handler.ServeHTTP(evilOrigin, request)
	if got := evilOrigin.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("untrusted CORS origin was reflected: %q", got)
	}

	for _, email := range []string{"budi@perusahaan.com", "unknown@example.com"} {
		reset := httptest.NewRecorder()
		body := bytes.NewBufferString(`{"email":"` + email + `"}`)
		request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/forgot-password", body)
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(reset, request)
		if reset.Code != http.StatusAccepted {
			t.Fatalf("forgot password for %s returned %d, want 202", email, reset.Code)
		}
	}
}

func TestProfileCannotChangeRole(t *testing.T) {
	server := httptest.NewServer(NewServer(repository.NewMemoryStore()).Router())
	defer server.Close()
	login := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email": "demo@tera.co.id", "password": "Demo1234",
	})
	token, _ := login.Data["token"].(string)
	response := requestJSON(t, server.Client(), http.MethodPut, server.URL+"/api/v1/auth/profile", token, map[string]any{
		"name": "Demo Owner", "role": "Administrator",
	})
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("role mutation status=%d, want 422; body=%s", response.StatusCode, response.Body)
	}
}

func TestLoginRateLimitAfterFiveFailures(t *testing.T) {
	server := httptest.NewServer(NewServer(repository.NewMemoryStore()).Router())
	defer server.Close()
	for attempt := 1; attempt <= 6; attempt++ {
		response := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/login", "", map[string]any{
			"email": "demo@tera.co.id", "password": "wrong-password",
		})
		want := http.StatusUnauthorized
		if attempt == 6 {
			want = http.StatusTooManyRequests
		}
		if response.StatusCode != want {
			t.Fatalf("attempt %d status=%d, want %d", attempt, response.StatusCode, want)
		}
	}
}

type captureMailer struct{ link string }

func (m *captureMailer) SendPasswordReset(_ context.Context, _ string, link string) error {
	m.link = link
	return nil
}

func TestPasswordResetTokenIsExpiringAndOneUse(t *testing.T) {
	t.Setenv("TERA_ENABLE_PASSWORD_RESET", "true")
	mailer := &captureMailer{}
	server := httptest.NewServer(NewServerWithMailer(repository.NewMemoryStore(), mailer).Router())
	defer server.Close()

	forgot := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/forgot-password", "", map[string]any{
		"email": "demo@tera.co.id",
	})
	if forgot.StatusCode != http.StatusAccepted || mailer.link == "" {
		t.Fatalf("forgot password status=%d link=%q", forgot.StatusCode, mailer.link)
	}
	parsed, err := url.Parse(mailer.link)
	if err != nil {
		t.Fatal(err)
	}
	token := parsed.Query().Get("token")
	resetBody := map[string]any{"token": token, "password": "NewDemo123"}
	reset := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/reset-password", "", resetBody)
	if reset.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status=%d body=%s", reset.StatusCode, reset.Body)
	}
	reuse := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/reset-password", "", resetBody)
	if reuse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reused reset status=%d, want 401", reuse.StatusCode)
	}
	login := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email": "demo@tera.co.id", "password": "NewDemo123",
	})
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login with reset password status=%d body=%s", login.StatusCode, login.Body)
	}
}

func TestSessionPersistsAcrossServerInstances(t *testing.T) {
	store := repository.NewMemoryStore()
	first := httptest.NewServer(NewServer(store).Router())
	defer first.Close()
	login := requestJSON(t, first.Client(), http.MethodPost, first.URL+"/api/v1/auth/login", "", map[string]any{
		"email": "demo@tera.co.id", "password": "Demo1234",
	})
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d body=%s", login.StatusCode, login.Body)
	}
	token, _ := login.Data["token"].(string)

	second := httptest.NewServer(NewServer(store).Router())
	defer second.Close()
	me := requestJSON(t, second.Client(), http.MethodGet, second.URL+"/api/v1/auth/me", token, nil)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("session did not survive server restart: status=%d body=%s", me.StatusCode, me.Body)
	}
}

func TestListEndpointsReturnPaginationMetadata(t *testing.T) {
	server := httptest.NewServer(NewServer(repository.NewMemoryStore()).Router())
	defer server.Close()
	login := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email": "demo@tera.co.id", "password": "Demo1234",
	})
	token, _ := login.Data["token"].(string)

	response := requestJSON(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/invoices?page=2&perPage=3&q=PT&sort=number&order=asc", token, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list invoices: status=%d body=%s", response.StatusCode, response.Body)
	}
	rows, _ := response.RawData.([]any)
	if len(rows) > 3 {
		t.Fatalf("received %d rows with perPage=3", len(rows))
	}
	if number(response.Meta["page"]) != 2 || number(response.Meta["perPage"]) != 3 || number(response.Meta["total"]) == 0 {
		t.Fatalf("unexpected pagination metadata: %#v", response.Meta)
	}

	badSort := requestJSON(t, server.Client(), http.MethodGet,
		server.URL+"/api/v1/invoices?sort=drop_table", token, nil)
	if badSort.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid sort status=%d, want 422", badSort.StatusCode)
	}
}

type jsonResponse struct {
	StatusCode int
	Data       map[string]any
	RawData    any
	Meta       map[string]any
	Body       string
}

func requestJSON(t *testing.T, client *http.Client, method, url, token string, body any) jsonResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	rawBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	result := jsonResponse{StatusCode: response.StatusCode, Body: string(rawBody)}
	if len(rawBody) == 0 {
		return result
	}
	var envelope map[string]any
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		t.Fatalf("decode response %q: %v", rawBody, err)
	}
	result.RawData = envelope["data"]
	result.Data, _ = envelope["data"].(map[string]any)
	result.Meta, _ = envelope["meta"].(map[string]any)
	return result
}

func number(value any) int64 {
	n, _ := value.(float64)
	return int64(n)
}

// The audit-log endpoint must return events newest-first. The handler used to
// walk the store's slice backwards, which silently double-reversed the order
// once the store started returning newest-first — the UI then rendered the
// trail oldest-first. This pins the contract at the HTTP boundary.
func TestAuditLogEndpointReturnsNewestFirst(t *testing.T) {
	server := httptest.NewServer(NewServer(repository.NewMemoryStore()).Router())
	defer server.Close()

	login := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email": "demo@tera.co.id", "password": "Demo1234",
	})
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", login.StatusCode, login.Body)
	}
	token, _ := login.Data["token"].(string)

	res := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/audit-logs", token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("audit-logs status = %d, body=%s", res.StatusCode, res.Body)
	}
	rows, ok := res.RawData.([]any)
	if !ok || len(rows) < 2 {
		t.Fatalf("expected a populated audit trail, got %#v", res.RawData)
	}

	previous := ""
	for i, raw := range rows {
		row, _ := raw.(map[string]any)
		stamp, _ := row["createdAt"].(string)
		if stamp == "" {
			t.Fatalf("row %d has no createdAt", i)
		}
		if previous != "" && stamp > previous {
			t.Fatalf("row %d (%s) is newer than its predecessor (%s); expected newest-first",
				i, stamp, previous)
		}
		previous = stamp
	}
}
