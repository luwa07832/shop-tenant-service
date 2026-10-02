package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/shop-tenant-service/internal/store"
)

// NewRouter wires the public HTTP surface described in README.md.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(recoveryHandler)

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "storage_unavailable", "message": "database is not available"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	v1 := router.Group("/v1")
	{
		v1.POST("/tenants", createTenant(st))
		v1.GET("/tenants/:id", getTenant(st))
		v1.PUT("/tenants/:id", updateTenant(st))
		v1.PATCH("/tenants/:id", updateTenant(st))
		v1.PUT("/tenants/:id/quotas", putQuotas(st))
		v1.GET("/tenants/:id/quotas", getQuotas(st))
		v1.GET("/tenants/:id/usage", getUsage(st))
		v1.POST("/tenants/:id/throttle/check", postThrottleCheck(st))
	}
	router.POST("/v1/metering/events", postMeteringEvent(st))

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "route_not_found", "message": "no route matches this path"}})
	})
	return router
}
