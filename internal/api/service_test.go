package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/luwa07832/shop-tenant-service/internal/store"
)

type apiHarness struct {
	t      *testing.T
	router http.Handler
}

func newHarness(t *testing.T) *apiHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &apiHarness{t: t, router: NewRouter(st)}
}

func (h *apiHarness) do(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			h.t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
		}
	}
	return rec.Code, decoded
}

func mustCode(t *testing.T, got, want int, body map[string]any) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d, body = %v", got, want, body)
	}
}

func asMap(t *testing.T, value any) map[string]any {
	t.Helper()
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T (%v)", value, value)
	}
	return m
}

func asSlice(t *testing.T, value any) []any {
	t.Helper()
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("expected array, got %T (%v)", value, value)
	}
	return list
}

func findQuota(t *testing.T, list []any, resource string) map[string]any {
	t.Helper()
	for _, item := range list {
		quota := asMap(t, item)
		if quota["resource"] == resource {
			return quota
		}
	}
	t.Fatalf("quota for %s not found in %v", resource, list)
	return nil
}

func createActiveTenant(h *apiHarness, id string) {
	h.t.Helper()
	code, body := h.do(http.MethodPost, "/v1/tenants", map[string]any{
		"id": id, "name": "Shop " + id, "status": "active", "plan_ref": "plan-basic",
	})
	mustCode(h.t, code, http.StatusCreated, body)
}

func TestTenantLifecycleAndIsolation(t *testing.T) {
	h := newHarness(t)

	code, body := h.do(http.MethodPost, "/v1/tenants", map[string]any{
		"id": "t1", "name": "First Shop", "status": "active", "plan_ref": "plan-pro",
	})
	mustCode(t, code, http.StatusCreated, body)
	if body["id"] != "t1" || body["status"] != "active" || body["plan_ref"] != "plan-pro" {
		t.Fatalf("unexpected tenant body: %v", body)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/t1", nil)
	mustCode(t, code, http.StatusOK, body)
	if body["name"] != "First Shop" {
		t.Fatalf("profile unreadable: %v", body)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/missing", nil)
	mustCode(t, code, http.StatusNotFound, body)
	errObj := asMap(t, body["error"])
	if errObj["code"] != "tenant_not_found" {
		t.Fatalf("code = %v", errObj)
	}
	if len(errObj) != 2 {
		t.Fatalf("error object must hold only code and message: %v", errObj)
	}

	code, body = h.do(http.MethodPost, "/v1/tenants", map[string]any{
		"id": "t2", "name": "Second", "status": "weird", "plan_ref": "plan-pro",
	})
	mustCode(t, code, http.StatusBadRequest, body)
	if asMap(t, body["error"])["code"] != "invalid_tenant_input" {
		t.Fatalf("code = %v", body["error"])
	}

	code, body = h.do(http.MethodPost, "/v1/tenants", map[string]any{
		"id": "t1", "name": "Dup", "status": "provisioning", "plan_ref": "plan-pro",
	})
	if code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d", code)
	}

	code, body = h.do(http.MethodPut, "/v1/tenants/t1", map[string]any{"status": "suspended"})
	mustCode(t, code, http.StatusOK, body)
	if body["status"] != "suspended" {
		t.Fatalf("status = %v", body["status"])
	}

	code, body = h.do(http.MethodPut, "/v1/tenants/t1", map[string]any{"status": "provisioning"})
	if code != http.StatusConflict {
		t.Fatalf("illegal transition status = %d body = %v", code, body)
	}
	if asMap(t, body["error"])["code"] != "invalid_tenant_input" {
		t.Fatalf("code = %v", body["error"])
	}

	code, body = h.do(http.MethodPut, "/v1/tenants/t1", map[string]any{"status": "active"})
	mustCode(t, code, http.StatusOK, body)
}

func TestQuotaUsageAndMeteringFlow(t *testing.T) {
	h := newHarness(t)
	createActiveTenant(h, "qa")

	code, body := h.do(http.MethodPut, "/v1/tenants/qa/quotas", map[string]any{
		"period": "2026-10", "resource": "orders", "limit": 100,
	})
	mustCode(t, code, http.StatusOK, body)
	if body["limit"].(float64) != 100 || body["used"].(float64) != 0 ||
		body["remaining"].(float64) != 100 || body["status"] != "ok" {
		t.Fatalf("quota = %v", body)
	}

	code, body = h.do(http.MethodPost, "/v1/metering/events", map[string]any{
		"tenant_id": "qa", "event_no": "e1", "period": "2026-10",
		"resource": "orders", "amount": 90, "event_time": "2026-10-01T00:00:00Z",
	})
	mustCode(t, code, http.StatusCreated, body)
	if body["remaining"].(float64) != 10 || body["status"] != "warning" {
		t.Fatalf("quota after event = %v", body)
	}

	code, body = h.do(http.MethodPost, "/v1/metering/events", map[string]any{
		"tenant_id": "qa", "event_no": "e1", "period": "2026-10",
		"resource": "orders", "amount": 90, "event_time": "2026-10-01T00:00:00Z",
	})
	if code != http.StatusConflict || asMap(t, body["error"])["code"] != "duplicate_metering_event" {
		t.Fatalf("duplicate: %d %v", code, body)
	}

	code, body = h.do(http.MethodPost, "/v1/metering/events", map[string]any{
		"tenant_id": "qa", "event_no": "e2", "period": "2026-10",
		"resource": "orders", "amount": 20, "event_time": "2026-10-01T00:00:00Z",
	})
	if code != http.StatusConflict || asMap(t, body["error"])["code"] != "quota_exceeded" {
		t.Fatalf("over limit: %d %v", code, body)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/qa/quotas?period=2026-10", nil)
	mustCode(t, code, http.StatusOK, body)
	quotas := asSlice(t, body["quotas"])
	orders := asMap(t, findQuota(t, quotas, "orders"))
	if orders["used"].(float64) != 90 || orders["remaining"].(float64) != 10 || orders["status"] != "warning" {
		t.Fatalf("quota read = %v", orders)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/qa/usage?period=2026-10", nil)
	mustCode(t, code, http.StatusOK, body)
	usage := asSlice(t, body["usage"])
	ordersUsage := asMap(t, findQuota(t, usage, "orders"))
	if ordersUsage["used"].(float64) != orders["used"].(float64) ||
		ordersUsage["remaining"].(float64) != orders["remaining"].(float64) ||
		ordersUsage["status"] != orders["status"] {
		t.Fatalf("usage and quota disagree: %v vs %v", ordersUsage, orders)
	}

	code, body = h.do(http.MethodPut, "/v1/tenants/qa/quotas", map[string]any{
		"period": "2026-10", "resource": "orders", "limit": 80,
	})
	mustCode(t, code, http.StatusOK, body)
	if body["used"].(float64) != 90 || body["remaining"].(float64) != -10 || body["status"] != "exceeded" {
		t.Fatalf("quota after revision = %v", body)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/unknown/quotas?period=2026-10", nil)
	if code != http.StatusNotFound || asMap(t, body["error"])["code"] != "tenant_not_found" {
		t.Fatalf("cross tenant quota: %d %v", code, body)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/qa/quotas?period=1999-01", nil)
	if code != http.StatusNotFound || asMap(t, body["error"])["code"] != "quota_not_found" {
		t.Fatalf("missing quota: %d %v", code, body)
	}
}

func TestThrottleChecksAreIdempotentAndAtomic(t *testing.T) {
	h := newHarness(t)
	createActiveTenant(h, "th")

	code, body := h.do(http.MethodPut, "/v1/tenants/th/quotas", map[string]any{
		"period": "2026-10", "resource": "calls", "limit": 10,
	})
	mustCode(t, code, http.StatusOK, body)

	check := func(checkNo string, amount int64) (int, map[string]any) {
		return h.do(http.MethodPost, "/v1/tenants/th/throttle/check", map[string]any{
			"check_no": checkNo, "period": "2026-10", "resource": "calls", "amount": amount,
		})
	}

	code, body = check("c1", 4)
	mustCode(t, code, http.StatusOK, body)
	if body["allowed"] != true || body["remaining"].(float64) != 6 || body["status"] != "ok" {
		t.Fatalf("first check = %v", body)
	}

	code, body = check("c1", 4)
	mustCode(t, code, http.StatusOK, body)
	if body["allowed"] != true || body["remaining"].(float64) != 6 {
		t.Fatalf("repeated check must not deduct twice: %v", body)
	}

	code, body = check("c2", 7)
	mustCode(t, code, http.StatusOK, body)
	if body["allowed"] != false || body["remaining"].(float64) != 6 || body["status"] != "exceeded" {
		t.Fatalf("over-limit check = %v", body)
	}

	code, body = check("c3", 6)
	mustCode(t, code, http.StatusOK, body)
	if body["allowed"] != true || body["remaining"].(float64) != 0 || body["status"] != "warning" {
		t.Fatalf("exact-fit check = %v", body)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/th/usage?period=2026-10", nil)
	mustCode(t, code, http.StatusOK, body)
	usage := asMap(t, findQuota(t, asSlice(t, body["usage"]), "calls"))
	if usage["used"].(float64) != 10 || usage["remaining"].(float64) != 0 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestConcurrentThrottleChecksNeverOverspend(t *testing.T) {
	h := newHarness(t)
	createActiveTenant(h, "cc")

	code, body := h.do(http.MethodPut, "/v1/tenants/cc/quotas", map[string]any{
		"period": "2026-10", "resource": "calls", "limit": 50,
	})
	mustCode(t, code, http.StatusOK, body)

	const goroutines = 20
	var wg sync.WaitGroup
	results := make(chan bool, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, payload := h.do(http.MethodPost, "/v1/tenants/cc/throttle/check", map[string]any{
				"check_no": "p" + string(rune('a'+i)), "period": "2026-10",
				"resource": "calls", "amount": 5,
			})
			if status != http.StatusOK {
				t.Errorf("status = %d body = %v", status, payload)
				results <- false
				return
			}
			results <- payload["allowed"] == true
		}(i)
	}
	wg.Wait()
	close(results)

	allowed := 0
	for ok := range results {
		if ok {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed checks = %d, want exactly 10", allowed)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/cc/usage?period=2026-10", nil)
	mustCode(t, code, http.StatusOK, body)
	usage := asMap(t, findQuota(t, asSlice(t, body["usage"]), "calls"))
	if usage["used"].(float64) != 50 {
		t.Fatalf("used = %v, want 50", usage["used"])
	}
}

func TestSuspendedTenantRejectsWrites(t *testing.T) {
	h := newHarness(t)
	createActiveTenant(h, "sp")

	code, body := h.do(http.MethodPut, "/v1/tenants/sp/quotas", map[string]any{
		"period": "2026-10", "resource": "orders", "limit": 10,
	})
	mustCode(t, code, http.StatusOK, body)

	code, body = h.do(http.MethodPut, "/v1/tenants/sp", map[string]any{"status": "suspended"})
	mustCode(t, code, http.StatusOK, body)

	code, body = h.do(http.MethodPost, "/v1/metering/events", map[string]any{
		"tenant_id": "sp", "event_no": "e1", "period": "2026-10",
		"resource": "orders", "amount": 1, "event_time": "2026-10-02T00:00:00Z",
	})
	if code != http.StatusForbidden || asMap(t, body["error"])["code"] != "tenant_suspended" {
		t.Fatalf("suspended event: %d %v", code, body)
	}

	code, body = h.do(http.MethodPost, "/v1/tenants/sp/throttle/check", map[string]any{
		"check_no": "c1", "period": "2026-10", "resource": "orders", "amount": 1,
	})
	if code != http.StatusForbidden || asMap(t, body["error"])["code"] != "tenant_suspended" {
		t.Fatalf("suspended throttle: %d %v", code, body)
	}

	code, body = h.do(http.MethodGet, "/v1/tenants/sp/quotas?period=2026-10", nil)
	mustCode(t, code, http.StatusOK, body)
}

func TestErrorShapeNeverLeaksInternals(t *testing.T) {
	h := newHarness(t)
	probes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/v1/tenants", map[string]any{"id": "x"}},
		{http.MethodPost, "/v1/tenants", map[string]any{"id": "x", "name": "X", "status": "bad", "plan_ref": "p"}},
		{http.MethodGet, "/v1/tenants/nope", nil},
		{http.MethodGet, "/v1/tenants/nope/quotas?period=2026-10", nil},
		{http.MethodGet, "/v1/tenants/nope/usage?period=2026-10", nil},
		{http.MethodPut, "/v1/tenants/nope/quotas", map[string]any{"period": "2026-10", "resource": "r", "limit": 1}},
		{http.MethodPost, "/v1/tenants/nope/throttle/check", map[string]any{"check_no": "c", "period": "2026-10", "resource": "r", "amount": 1}},
		{http.MethodPost, "/v1/metering/events", map[string]any{"tenant_id": "nope", "event_no": "e", "period": "2026-10", "resource": "r", "amount": 1, "event_time": "2026-10-01T00:00:00Z"}},
	}
	forbidden := []string{"SELECT", "INSERT", "sql:", ".go:", "goroutine", "/internal/", ".db"}
	for _, probe := range probes {
		code, body := h.do(probe.method, probe.path, probe.body)
		if code == http.StatusOK || code == http.StatusCreated {
			t.Fatalf("%s %s unexpectedly succeeded: %v", probe.method, probe.path, body)
		}
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("%s %s missing error object: %v", probe.method, probe.path, body)
		}
		if len(errObj) != 2 {
			t.Fatalf("%s %s error must only contain code and message: %v", probe.method, probe.path, errObj)
		}
		codeText, _ := errObj["code"].(string)
		message, _ := errObj["message"].(string)
		if codeText == "" || message == "" {
			t.Fatalf("%s %s empty code/message: %v", probe.method, probe.path, errObj)
		}
		upper := message
		for _, marker := range forbidden {
			if strings.Contains(upper, marker) || strings.Contains(message, marker) {
				t.Fatalf("%s %s leaks %q in message: %q", probe.method, probe.path, marker, message)
			}
		}
	}
}
