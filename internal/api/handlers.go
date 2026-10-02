package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/shop-tenant-service/internal/store"
)

type createTenantRequest struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	PlanRef  string `json:"plan_ref"`
	Plan     string `json:"plan"`
	PlanID   string `json:"plan_id"`
}

func createTenant(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createTenantRequest
		if !decodeBody(c, &req) {
			return
		}
		id := firstNonEmpty(req.ID, req.TenantID)
		planRef := firstNonEmpty(req.PlanRef, req.Plan, req.PlanID)
		name := strings.TrimSpace(req.Name)
		status := strings.TrimSpace(req.Status)
		if !validIdentifier(id) || name == "" || len(name) > 256 ||
			status == "" || len(status) > 32 ||
			planRef == "" || len(planRef) > 128 {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "tenant identifier, name, initial status or plan reference is invalid")
			return
		}
		switch status {
		case "provisioning", "active", "suspended", "archived":
		default:
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "tenant status is not one of the allowed values")
			return
		}
		tenant, err := st.CreateTenant(store.CreateTenantParams{
			ID:      id,
			Name:    name,
			Status:  status,
			PlanRef: planRef,
			Now:     nowRFC3339(),
		})
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusCreated, tenant)
	}
}

func getTenant(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant, err := st.GetTenant(c.Param("id"))
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, tenant)
	}
}

type updateTenantRequest struct {
	Name    *string `json:"name"`
	Status  *string `json:"status"`
	PlanRef *string `json:"plan_ref"`
	Plan    *string `json:"plan"`
}

func updateTenant(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req updateTenantRequest
		if !decodeBody(c, &req) {
			return
		}
		params := store.UpdateTenantParams{ID: c.Param("id"), Now: nowRFC3339()}
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" || len(name) > 256 {
				writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "tenant name is invalid")
				return
			}
			params.Name = &name
		}
		if req.PlanRef != nil || req.Plan != nil {
			planRef := firstNonEmpty(deref(req.PlanRef), deref(req.Plan))
			if planRef == "" || len(planRef) > 128 {
				writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "plan reference is invalid")
				return
			}
			params.PlanRef = &planRef
		}
		if req.Status != nil {
			status := strings.TrimSpace(*req.Status)
			switch status {
			case "provisioning", "active", "suspended", "archived":
			default:
				writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "tenant status is not one of the allowed values")
				return
			}
			params.Status = &status
		}
		tenant, err := st.UpdateTenant(params)
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, tenant)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type quotaLineInput struct {
	Resource   string `json:"resource"`
	ResourceID string `json:"resource_id"`
	Limit      *int64 `json:"limit"`
	LimitValue *int64 `json:"limit_value"`
}

type putQuotasRequest struct {
	Period        string           `json:"period"`
	BillingPeriod string           `json:"billing_period"`
	Resource      string           `json:"resource"`
	ResourceID    string           `json:"resource_id"`
	Limit         *int64           `json:"limit"`
	LimitValue    *int64           `json:"limit_value"`
	Quotas        []quotaLineInput `json:"quotas"`
	Resources     []quotaLineInput `json:"resources"`
}

func putQuotas(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req putQuotasRequest
		if !decodeBody(c, &req) {
			return
		}
		period := firstNonEmpty(req.Period, req.BillingPeriod)
		if !validPeriod(period) {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "billing period is missing or invalid")
			return
		}
		var lines []quotaLineInput
		if len(req.Quotas) > 0 {
			lines = req.Quotas
		} else if len(req.Resources) > 0 {
			lines = req.Resources
		} else if req.Resource != "" || req.ResourceID != "" || req.Limit != nil || req.LimitValue != nil {
			lines = []quotaLineInput{{Resource: firstNonEmpty(req.Resource, req.ResourceID), Limit: firstLimit(req.Limit, req.LimitValue)}}
		}
		if len(lines) == 0 {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "resource and limit are required")
			return
		}
		type applied struct {
			resource string
			quota    store.Quota
		}
		results := make([]applied, 0, len(lines))
		for _, line := range lines {
			resource := firstNonEmpty(line.Resource, line.ResourceID)
			limit := firstLimit(line.Limit, line.LimitValue)
			if !validIdentifier(resource) || limit == nil || *limit < 0 {
				writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "resource name or limit is invalid")
				return
			}
			quota, err := st.PutQuota(c.Param("id"), period, resource, *limit, nowRFC3339())
			if err != nil {
				mapStoreError(c, err)
				return
			}
			results = append(results, applied{resource: resource, quota: quota})
		}
		if len(results) == 1 && len(lines) == 1 && len(req.Quotas) == 0 && len(req.Resources) == 0 {
			c.JSON(http.StatusOK, results[0].quota)
			return
		}
		out := make([]store.Quota, 0, len(results))
		for _, result := range results {
			out = append(out, result.quota)
		}
		c.JSON(http.StatusOK, gin.H{"period": period, "quotas": out})
	}
}

func firstLimit(a, b *int64) *int64 {
	if a != nil {
		return a
	}
	return b
}

func getQuotas(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		period := strings.TrimSpace(c.Query("period"))
		if period == "" {
			period = strings.TrimSpace(c.Query("billing_period"))
		}
		if !validPeriod(period) {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "period query parameter is required")
			return
		}
		quotas, err := st.ListQuotas(c.Param("id"), period)
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"period": period, "quotas": quotas})
	}
}

type meteringEventRequest struct {
	TenantID      string `json:"tenant_id"`
	Tenant        string `json:"tenant"`
	ID            string `json:"id"`
	EventNo       string `json:"event_no"`
	EventID       string `json:"event_id"`
	Number        string `json:"number"`
	Period        string `json:"period"`
	BillingPeriod string `json:"billing_period"`
	Resource      string `json:"resource"`
	ResourceID    string `json:"resource_id"`
	Amount        *int64 `json:"amount"`
	Quantity      *int64 `json:"quantity"`
	EventTime     string `json:"event_time"`
	Time          string `json:"time"`
	Timestamp     string `json:"timestamp"`
	OccurredAt    string `json:"occurred_at"`
}

func postMeteringEvent(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req meteringEventRequest
		if !decodeBody(c, &req) {
			return
		}
		tenantID := firstNonEmpty(req.TenantID, req.Tenant, req.ID)
		eventNo := firstNonEmpty(req.EventNo, req.EventID, req.Number)
		period := firstNonEmpty(req.Period, req.BillingPeriod)
		resource := firstNonEmpty(req.Resource, req.ResourceID)
		amount := firstLimit(req.Amount, req.Quantity)
		eventTime := firstNonEmpty(req.EventTime, req.Time, req.Timestamp, req.OccurredAt)
		if strings.TrimSpace(eventTime) == "" {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "event_time is required")
			return
		}
		parsed, err := time.Parse(time.RFC3339, eventTime)
		if err != nil {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "event_time must be an RFC3339 timestamp")
			return
		}
		if !validIdentifier(tenantID) || !validIdentifier(eventNo) || !validPeriod(period) ||
			!validIdentifier(resource) || amount == nil || *amount < 0 {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "tenant, event number, period, resource or amount is invalid")
			return
		}
		quota, err := st.RecordMeteringEvent(store.MeteringEvent{
			TenantID:   tenantID,
			EventNo:    eventNo,
			Period:     period,
			Resource:   resource,
			Amount:     *amount,
			OccurredAt: parsed.UTC().Format(time.RFC3339),
		})
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusCreated, quota)
	}
}

func getUsage(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		period := strings.TrimSpace(c.Query("period"))
		if period == "" {
			period = strings.TrimSpace(c.Query("billing_period"))
		}
		if !validPeriod(period) {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "period query parameter is required")
			return
		}
		usage, err := st.Usage(c.Param("id"), period)
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"period": period, "usage": usage})
	}
}

type throttleCheckRequest struct {
	CheckNo       string `json:"check_no"`
	CheckID       string `json:"check_id"`
	RequestID     string `json:"request_id"`
	Period        string `json:"period"`
	BillingPeriod string `json:"billing_period"`
	Resource      string `json:"resource"`
	ResourceID    string `json:"resource_id"`
	Amount        *int64 `json:"amount"`
	Quantity      *int64 `json:"quantity"`
}

func postThrottleCheck(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req throttleCheckRequest
		if !decodeBody(c, &req) {
			return
		}
		checkNo := firstNonEmpty(req.CheckNo, req.CheckID, req.RequestID)
		period := firstNonEmpty(req.Period, req.BillingPeriod)
		resource := firstNonEmpty(req.Resource, req.ResourceID)
		amount := firstLimit(req.Amount, req.Quantity)
		if !validIdentifier(checkNo) || !validPeriod(period) || !validIdentifier(resource) ||
			amount == nil || *amount < 0 {
			writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "check number, period, resource or amount is invalid")
			return
		}
		result, err := st.CheckThrottle(store.ThrottleCheckParams{
			TenantID: c.Param("id"),
			CheckNo:  checkNo,
			Period:   period,
			Resource: resource,
			Amount:   *amount,
			Now:      nowRFC3339(),
		})
		if err != nil {
			mapStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}
