package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
)

// JWTValidator validates JWT tokens and extracts user context
func JWTValidator(auth interfaces.IAuth) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Extract Bearer token from Authorization header
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"type":   "https://kube-sbt.io/problems/unauthorized",
				"title":  "Unauthorized",
				"status": http.StatusUnauthorized,
				"detail": "Missing Authorization header",
			})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"type":   "https://kube-sbt.io/problems/unauthorized",
				"title":  "Unauthorized",
				"status": http.StatusUnauthorized,
				"detail": "Invalid Authorization header format",
			})
			return
		}

		token := parts[1]

		// Validate token with Ory Kratos
		session, err := auth.ValidateSession(c.Request.Context(), token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"type":   "https://kube-sbt.io/problems/unauthorized",
				"title":  "Unauthorized",
				"status": http.StatusUnauthorized,
				"detail": "Invalid or expired token",
			})
			return
		}

		// Inject user context into request
		c.Set("user_id", session.Identity.ID)
		c.Set("tenant_id", session.Identity.Traits["tenant_id"])
		c.Set("session", session)

		c.Next()
	}
}

// TenantContextInjector extracts tenant_id from path and validates against JWT
func TenantContextInjector() gin.HandlerFunc {
	return func(c *gin.Context) {
		pathTenantID := c.Param("tenantID")
		jwtTenantID, exists := c.Get("tenant_id")

		if pathTenantID != "" && exists {
			// Validate path tenant_id matches JWT tenant_id
			if pathTenantID != jwtTenantID.(string) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"type":   "https://kube-sbt.io/problems/forbidden",
					"title":  "Forbidden",
					"status": http.StatusForbidden,
					"detail": "Tenant ID mismatch",
				})
				return
			}
		}

		// Inject namespace (tenant_id) for OpenMeter calls
		if exists {
			c.Set("namespace", jwtTenantID.(string))
		}

		c.Next()
	}
}

// RBACValidator checks if user has required role
func RBACValidator(requiredRole string) gin.HandlerFunc {
	return func(c *gin.Context) {
		session, exists := c.Get("session")
		if !exists {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"type":   "https://kube-sbt.io/problems/unauthorized",
				"title":  "Unauthorized",
				"status": http.StatusUnauthorized,
				"detail": "No session found",
			})
			return
		}

		// Extract roles from session traits
		roles, ok := session.(map[string]interface{})["traits"].(map[string]interface{})["roles"].([]string)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"type":   "https://kube-sbt.io/problems/forbidden",
				"title":  "Forbidden",
				"status": http.StatusForbidden,
				"detail": "Insufficient permissions",
			})
			return
		}

		// Check if required role exists
		hasRole := false
		for _, role := range roles {
			if role == requiredRole {
				hasRole = true
				break
			}
		}

		if !hasRole {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"type":   "https://kube-sbt.io/problems/forbidden",
				"title":  "Forbidden",
				"status": http.StatusForbidden,
				"detail": "Insufficient permissions",
			})
			return
		}

		c.Next()
	}
}
