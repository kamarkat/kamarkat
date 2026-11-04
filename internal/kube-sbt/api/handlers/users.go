package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
)

// UserHandler handles user management endpoints
type UserHandler struct {
	auth interfaces.IAuth
}

// NewUserHandler creates a new user handler
func NewUserHandler(auth interfaces.IAuth) *UserHandler {
	return &UserHandler{auth: auth}
}

// CreateUser handles POST /api/v1/tenants/{tenantID}/users
func (h *UserHandler) CreateUser(c *gin.Context) {
	tenantID := c.Param("tenantID")

	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=8"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"type":   "https://kube-sbt.io/problems/bad-request",
			"title":  "Bad Request",
			"status": http.StatusBadRequest,
			"detail": err.Error(),
		})
		return
	}

	// Create the user at the identity provider
	//
	// TenantID is set on the FIELD, not smuggled through Traits. createIdentity
	// reads u.TenantID and writes it to metadata_public (ADR-010); it never looks
	// at Traits. Passing it as a trait therefore did two wrong things at once —
	// the value was dropped, and the identity schema admits only `email`, so the
	// extra trait could not have been stored even if it had been read.
	//
	// The cost was invisible: this endpoint returned 201 and produced a user with
	// no tenant binding at all. On 2026-09-02 such a user could authenticate
	// against the identity provider and was then rejected by every service with
	// "Token missing required tenant_id claim", because the claim the auth-proxy
	// derives from metadata_public was never there to derive.
	user := models.User{
		Email:    req.Email,
		Password: req.Password,
		TenantID: tenantID,
		Traits: map[string]interface{}{
			"email": req.Email,
		},
	}

	idpUser, err := h.auth.CreateUser(c.Request.Context(), user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"type":   "https://kube-sbt.io/problems/internal-error",
			"title":  "Internal Server Error",
			"status": http.StatusInternalServerError,
			"detail": "Failed to create user in identity provider",
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":         idpUser.ID,
		"email":      idpUser.Email,
		"created_at": idpUser.CreatedAt,
	})
}

// GetUser handles GET /api/v1/tenants/{tenantID}/users/{userID}
func (h *UserHandler) GetUser(c *gin.Context) {
	userID := c.Param("userID")

	user, err := h.auth.GetUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"type":   "https://kube-sbt.io/problems/not-found",
			"title":  "Not Found",
			"status": http.StatusNotFound,
			"detail": "User not found",
		})
		return
	}

	c.JSON(http.StatusOK, user)
}

// UpdateUser handles PUT /api/v1/tenants/{tenantID}/users/{userID}
func (h *UserHandler) UpdateUser(c *gin.Context) {
	userID := c.Param("userID")

	var req struct {
		Email    string                 `json:"email"`
		Traits   map[string]interface{} `json:"traits"`
		Metadata map[string]string      `json:"metadata"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"type":   "https://kube-sbt.io/problems/bad-request",
			"title":  "Bad Request",
			"status": http.StatusBadRequest,
			"detail": err.Error(),
		})
		return
	}

	updates := models.UserUpdates{
		Email:  &req.Email,
		Traits: req.Traits,
	}

	user, err := h.auth.UpdateUser(c.Request.Context(), userID, updates)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"type":   "https://kube-sbt.io/problems/internal-error",
			"title":  "Internal Server Error",
			"status": http.StatusInternalServerError,
			"detail": "Failed to update user",
		})
		return
	}

	c.JSON(http.StatusOK, user)
}

// DeleteUser handles DELETE /api/v1/tenants/{tenantID}/users/{userID}
func (h *UserHandler) DeleteUser(c *gin.Context) {
	userID := c.Param("userID")

	// Delete the user at the identity provider
	err := h.auth.DeleteUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"type":   "https://kube-sbt.io/problems/internal-error",
			"title":  "Internal Server Error",
			"status": http.StatusInternalServerError,
			"detail": "Failed to delete user",
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// ListUsers handles GET /api/v1/tenants/{tenantID}/users
func (h *UserHandler) ListUsers(c *gin.Context) {
	tenantID := c.Param("tenantID")

	// Parse pagination parameters
	page := c.DefaultQuery("page", "1")
	pageSize := c.DefaultQuery("page_size", "20")

	users, err := h.auth.ListUsers(c.Request.Context(), tenantID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"type":   "https://kube-sbt.io/problems/internal-error",
			"title":  "Internal Server Error",
			"status": http.StatusInternalServerError,
			"detail": "Failed to list users",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"users":     users,
		"page":      page,
		"page_size": pageSize,
	})
}
