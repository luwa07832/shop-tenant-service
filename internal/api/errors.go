package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/shop-tenant-service/internal/store"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func validIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

func validPeriod(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	return identifierPattern.MatchString(value)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func writeAPIError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// recoveryHandler keeps the published error shape even when a handler panics.
func recoveryHandler(c *gin.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if !c.Writer.Written() {
				writeAPIError(c, http.StatusInternalServerError, "internal_error", "request could not be completed")
			}
		}
	}()
	c.Next()
}

func badRequest(c *gin.Context) {
	writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "request body is missing or malformed")
}

// decodeBody reads a JSON object and rejects trailing or unknown fields.
func decodeBody(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		badRequest(c)
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		badRequest(c)
		return false
	}
	return true
}

// mapStoreError converts a storage-layer sentinel into the published error shape.
// Only stable codes and sanitized messages cross the boundary.
func mapStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrTenantNotFound):
		writeAPIError(c, http.StatusNotFound, "tenant_not_found", "tenant does not exist")
	case errors.Is(err, store.ErrQuotaNotFound):
		writeAPIError(c, http.StatusNotFound, "quota_not_found", "no quota is defined for this tenant, period and resource")
	case errors.Is(err, store.ErrInvalidInput):
		writeAPIError(c, http.StatusBadRequest, "invalid_tenant_input", "a field has an invalid value")
	case errors.Is(err, store.ErrIllegalStatus):
		writeAPIError(c, http.StatusConflict, "invalid_tenant_input", "requested tenant status transition is not allowed")
	case errors.Is(err, store.ErrQuotaExceeded):
		writeAPIError(c, http.StatusConflict, "quota_exceeded", "remaining quota is insufficient for this request")
	case errors.Is(err, store.ErrTenantSuspended):
		writeAPIError(c, http.StatusForbidden, "tenant_suspended", "tenant is suspended and cannot accept changes")
	case errors.Is(err, store.ErrTenantConflict):
		writeAPIError(c, http.StatusConflict, "tenant_already_exists", "a tenant with this identifier already exists")
	case errors.Is(err, store.ErrDuplicateEvent):
		writeAPIError(c, http.StatusConflict, "duplicate_metering_event", "a metering event with this event number was already recorded")
	default:
		writeAPIError(c, http.StatusInternalServerError, "internal_error", "request could not be completed")
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
