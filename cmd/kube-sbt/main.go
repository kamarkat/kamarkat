package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/api/handlers"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/api/middleware"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/interfaces"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/models"
	"github.com/kamarkat/kamarkat/internal/kube-sbt/providers/zitadel"
)

func main() {
	// Initialize providers with graceful retry logic
	authProviderRaw, err := retryWithBackoff(func() (interface{}, error) {
		return newAuthProvider()
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create auth provider after retries: %v", err))
	}
	authProvider := authProviderRaw.(interfaces.IAuth)

	// Initialize Gin router
	router := gin.New()

	// Global middleware
	router.Use(gin.Recovery())
	router.Use(middleware.RequestLogger())
	router.Use(middleware.CORS())
	router.Use(middleware.RateLimiter(100, 200)) // 100 req/sec burst, 200 max

	// Health check endpoint (no auth required)
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy"})
	})

	// API v1 routes with JWT validation
	v1 := router.Group("/api/v1")
	v1.Use(middleware.JWTValidator(authProvider))
	v1.Use(middleware.TenantContextInjector())
	v1.Use(middleware.RFC7807ErrorHandler())
	{
		// User management endpoints
		users := handlers.NewUserHandler(authProvider)
		v1.POST("/tenants/:tenantID/users", users.CreateUser)
		v1.GET("/tenants/:tenantID/users/:userID", users.GetUser)
		v1.PUT("/tenants/:tenantID/users/:userID", users.UpdateUser)
		v1.DELETE("/tenants/:tenantID/users/:userID", users.DeleteUser)
		v1.GET("/tenants/:tenantID/users", users.ListUsers)
	}

	// Admin endpoints (platform_admin RBAC required)
	admin := router.Group("/api/v1/admin")
	admin.Use(middleware.JWTValidator(authProvider))
	admin.Use(middleware.RBACValidator("platform_admin"))
	admin.Use(middleware.RFC7807ErrorHandler())
	{
		dlq := handlers.NewDLQHandler()
		admin.POST("/dlq/replay", middleware.RateLimiter(50, 100), dlq.ReplayEvent)

	}

	// Tenant identity provisioning (ADR-041: this service owns the identity
	// lifecycle; the Hub Operator orchestrates and calls it).
	//
	// On an INTERNAL path, guarded by NetworkPolicy rather than by a bearer
	// token. The caller is a controller, not a person, and the admin group above
	// authenticates a USER — putting a reconcile loop behind it would mean
	// minting and rotating a user credential for a machine, which is a worse
	// thing to own than an ingress rule. This mirrors the auth-proxy's own
	// /internal/ surface.
	//
	// The guarantee is therefore the NetworkPolicy beside this deployment: it
	// admits the operator and nothing else. Widening that policy widens this.
	//
	// Registered only when the configured provider can provision tenants — the
	// capability is optional, and a route that always answered "not implemented"
	// would read as an outage rather than a configuration.
	if provisioner, ok := authProvider.(interfaces.ITenantIdentityProvisioner); ok {
		internal := router.Group("/internal")
		internal.Use(middleware.RFC7807ErrorHandler())
		ti := handlers.NewTenantIdentityHandler(provisioner, nil)
		internal.POST("/tenants/:tenantId/identity", ti.EnsureIdentity)

		// Platform-level clients, of which the Kubernetes API server is the
		// motivating one: its client id is a process FLAG, so unlike a tenant's
		// gateway it cannot read the value at runtime and the application has to
		// exist before anything can name it.
		if papp, ok := authProvider.(handlers.PlatformAppProvisioner); ok {
			pa := handlers.NewPlatformAppHandler(papp)
			internal.POST("/platform/apps", pa.EnsureApp)
			internal.POST("/platform/projects", pa.EnsureProject)
			internal.POST("/platform/clients", pa.EnsureConfidentialClient)
			// Who may be issued a token for an application in a platform
			// project. Separate from creating the project because the credit
			// authority's admits one machine caller and the observability one
			// admits the box's operators (ADR-078 add.5 §7).
			internal.POST("/platform/projects/access", pa.EnsureProjectAccess)
		}
	}

	// Close self-service registration.
	//
	// The issuer allows it by default, and a self-registered account is given no
	// organisation — so it lands in the DEFAULT one, which is the platform's own,
	// where the administrators are. Tenants are onboarded by provisioning, which
	// creates an organisation and its owner deliberately, so this is not a door
	// that should be narrower here; it is one that should not exist.
	//
	// Asserted on every start because it is the issuer's default: a version
	// upgrade or a policy reset restores it silently, and a check that only ran
	// at installation would never notice.
	if closer, ok := authProvider.(interface {
		EnsureRegistrationClosed(context.Context) error
	}); ok {
		if err := closer.EnsureRegistrationClosed(context.Background()); err != nil {
			fmt.Printf("warning: could not close self-service registration: %v\n", err)
		}
	}

	// The issuer's outbound mail sender.
	//
	// ADR-060 (Amendment 2026-09-26): the issuer owns the credential lifecycle,
	// which it can only do if it can send mail. Tenant owners are now created
	// passwordless so an initialisation link is mailed to them; with no sender the
	// account is created, no mail arrives, and the owner cannot get in at all.
	// This is therefore a prerequisite for provisioning, not a notification
	// nicety.
	//
	// Reconciled here rather than left to the chart because the chart configures
	// SMTP through DefaultInstance, which the issuer reads exactly once — when the
	// FIRST instance is created. On a cluster whose instance already exists that
	// block configures nothing, silently. The chart covers a rebuilt box; this
	// covers the one already running, and asserting it on every start means the
	// two cannot disagree for long.
	//
	// Absent configuration is a no-op, not a failure: a box that has not been
	// given a sender yet should start and serve everything that does not depend on
	// mail. Failure to APPLY a sender that WAS configured is a warning for the
	// same reason every other assertion here is.
	if smtpHost := getEnv("ZITADEL_SMTP_HOST", ""); smtpHost != "" {
		if mailer, ok := authProvider.(interface {
			EnsureSMTP(context.Context, zitadel.SMTPConfig) error
		}); ok {
			cfg := zitadel.SMTPConfig{
				Host:          smtpHost,
				User:          getEnv("ZITADEL_SMTP_USER", ""),
				Password:      getEnv("ZITADEL_SMTP_PASSWORD", ""),
				TLS:           getEnv("ZITADEL_SMTP_TLS", "true") != "false",
				SenderAddress: getEnv("ZITADEL_SMTP_FROM", ""),
				SenderName:    getEnv("ZITADEL_SMTP_FROM_NAME", ""),
				ReplyTo:       getEnv("ZITADEL_SMTP_REPLY_TO", ""),
			}
			if err := mailer.EnsureSMTP(context.Background(), cfg); err != nil {
				fmt.Printf("warning: could not configure the issuer's mail sender; "+
					"tenant owners will be created with no way to receive their "+
					"initialisation mail: %v\n", err)
			}
		}
	}

	// The Kubernetes API server's OIDC client.
	//
	// Reconciled here rather than created once, because it is the ONE client the
	// platform cannot repair from configuration: its id is an API-server flag, so
	// a missing application means every kubectl login fails and fixing it needs a
	// control-plane rollout. Asserting it on every start makes that
	// unreachable — the application is recreated long before anyone notices.
	//
	// Idempotent: an existing application is found and its id returned, so this
	// does not churn the client id that the API server is already configured
	// with. Failure is a warning: identity being briefly unreachable must not
	// stop this service from serving everything that does not depend on it.
	if papp, ok := authProvider.(handlers.PlatformAppProvisioner); ok {
		if redirects := getEnv("KUBERNETES_OIDC_REDIRECT_URIS", ""); redirects != "" {
			uris := strings.Split(redirects, ",")
			for i := range uris {
				uris[i] = strings.TrimSpace(uris[i])
			}
			if clientID, err := papp.EnsurePlatformApp(context.Background(),
				getEnv("KUBERNETES_OIDC_APP_NAME", "kubernetes"), uris, nil); err != nil {
				fmt.Printf("warning: could not ensure the Kubernetes OIDC client: %v\n", err)
			} else {
				// Logged because the API server names this id in a flag, so an
				// operator comparing the two needs to see it without a console.
				fmt.Printf("kubernetes OIDC client ensured: %s\n", clientID)
			}
		}
	}

	// Ensure the platform administrator can actually sign in.
	//
	// Runs on every start because it converges rather than initialises: an issuer
	// that denies authentication to a user holding no role will refuse an
	// administrator whose account exists but whose grant was never made or was
	// later removed, and the error names a missing grant rather than a missing
	// person.
	//
	// Unset means "this environment does not bootstrap an administrator", which
	// is legitimate — an issuer without organisations has nothing to grant. It is
	// deliberately not fatal: identity being briefly unreachable must not stop
	// the API server from serving everything that does not depend on it.
	if adminEmail := getEnv("PLATFORM_ADMIN_EMAIL", ""); adminEmail != "" {
		if err := authProvider.CreateAdminUser(context.Background(), models.CreateAdminUserProps{
			Email: adminEmail,
			Name:  adminEmail,
			Role:  "admin",
		}); err != nil {
			fmt.Printf("warning: could not ensure the platform administrator %q: %v\n", adminEmail, err)
		} else {
			fmt.Printf("platform administrator ensured: %s\n", adminEmail)
		}
	}

	// Start HTTP server
	port := getEnv("PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		fmt.Printf("kube-sbt API server listening on :%s\n", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(fmt.Sprintf("failed to start server: %v", err))
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	fmt.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		panic(fmt.Sprintf("server forced to shutdown: %v", err))
	}

	fmt.Println("Server exited")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// retryWithBackoff executes a function with exponential backoff retry logic
// This implements the "stable-but-not-ready" pattern for graceful dependency handling
func retryWithBackoff(fn func() (interface{}, error)) (interface{}, error) {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	maxRetries := 30 // 5 minutes total with 10s intervals

	for i := 0; i < maxRetries; i++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}

		// Log the retry attempt
		fmt.Printf("Dependency connection failed (attempt %d/%d): %v, retrying in %v...\n", i+1, maxRetries, err, backoff)

		// Sleep with exponential backoff
		time.Sleep(backoff)

		// Increase backoff (exponential, capped at maxBackoff)
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	return nil, fmt.Errorf("max retries (%d) exceeded", maxRetries)
}

// newAuthProvider selects the identity provider from configuration.
//
// The interface was always pluggable; the WIRING was not — this call site named
// one product, so a second provider could not be selected without changing code
// and rebuilding. An abstraction that only a recompile can re-point is not one
// (ADR-059), so the choice is a setting.
//
// The Ory provider this selected between was removed with the Ory stack it
// spoke to. It remained the DEFAULT after that removal, so an unset variable
// started this service against Kratos at an address nothing serves, and the
// failure appeared as authentication errors rather than as a missing component.
// Zitadel is the default and the only accepted value; an unrecognised one is
// named rather than fallen back from.
func newAuthProvider() (interfaces.IAuth, error) {
	switch provider := getEnv("AUTH_PROVIDER", "zitadel"); provider {
	case "zitadel":
		// The issuer is the public URL, not an in-cluster Service address. It is
		// what relying parties are configured with and what the token's iss claim
		// must equal, and this provider verifies the discovery document agrees
		// before trusting any key from it.
		return zitadel.NewAuth(zitadel.Config{
			Issuer:        getEnv("OIDC_ISSUER_URL", ""),
			ServiceToken:  os.Getenv("IDENTITY_SERVICE_TOKEN"),
			PlatformOrgID: getEnv("PLATFORM_ORG_ID", ""),
			ProjectName:   getEnv("IDENTITY_PROJECT_NAME", "platform"),
		})

	default:
		// Naming the value rather than falling back. A typo that silently
		// selected a default would start the service against the wrong issuer,
		// and every token would then fail validation for reasons that point
		// anywhere but here.
		return nil, fmt.Errorf("unknown AUTH_PROVIDER %q: expected \"zitadel\"", provider)
	}
}
