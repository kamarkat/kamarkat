package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	authproxy "github.com/kamarkat/kamarkat/internal/auth-proxy"
)

func main() {
	cfg, err := authproxy.LoadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// No client registration on start.
	//
	// This used to register an OAuth client with Hydra's admin API before serving
	// anything, and exit if that failed. Zitadel has no dynamic client
	// registration (RFC 7591): clients are provisioned by the Tenant Identity
	// Service (ADR-041), so there is nothing for this process to register and
	// nothing whose absence should stop it starting.
	handler := authproxy.NewHandler(
		cfg.ZitadelIssuerURL,
		cfg.ZitadelInternalURL,
		cfg.JWKSFetchTimeout,
		cfg.ExpectedJWTAudience,
		cfg.AuthPublicBaseURL,
		cfg.MCPGatewayBaseURL,
	)

	// Readiness is the issuer being reachable, and it is reported rather than
	// asserted at startup.
	//
	// This used to exit if the first JWKS fetch failed. That was survivable when
	// the issuer was Hydra and an init container had already blocked until it was
	// up; against Zitadel it is not. Zitadel initialises its database on first
	// boot and is legitimately unreachable for minutes, so a hard exit turns a
	// dependency that has not arrived yet into CrashLoopBackOff on THIS
	// component -- which reads as auth-proxy being broken and hides the issuer
	// that actually is.
	//
	// Staying un-Ready expresses the same requirement without the misattribution:
	// the readiness probe fails, no traffic is routed here, and the pod's own
	// status names the reason. The retry loop then flips it Ready when the issuer
	// appears, with no restart.
	validator := authproxy.NewJWTValidatorWithHost(
		handler.JWKSURL(),
		handler.IssuerHost(),
		cfg.ExpectedJWTAudience,
		cfg.JWKSCacheTTL,
		cfg.JWKSFetchTimeout,
	)
	// A syntactically valid token that cannot verify: reaching a "cannot verify"
	// error means the fetch itself worked, which is the only thing being probed.
	const probeToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3QifQ.e30.test"
	go func() {
		for attempt := 1; ; attempt++ {
			_, err := validator.Validate(probeToken)
			if err == nil || !strings.Contains(err.Error(), "failed to fetch JWKS") {
				log.Println("JWKS reachable; reporting ready")
				handler.SetReady()
				return
			}
			if attempt == 1 || attempt%10 == 0 {
				log.Printf("JWKS not reachable yet (attempt %d): %v", attempt, err)
			}
			time.Sleep(5 * time.Second)
		}
	}()

	// Only the machine surfaces the Gateway routes here are served.
	//
	// /login, /consent, /userinfo and /oauth2/* are gone: they implemented
	// Hydra's login/consent challenge flow, and auth.<zone> now redirects
	// everything outside /.well-known/ and /internal/ to Zitadel, which hosts its
	// own login. Keeping them would have left endpoints that no request reaches
	// and that no longer have a backend.
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", handler.ProxyOpenIDConfiguration)
	mux.HandleFunc("/.well-known/oauth-authorization-server", handler.ServeAuthServerMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", handler.ServeAuthServerMetadata)
	mux.HandleFunc("/.well-known/jwks.json", handler.ProxyJWKS)
	mux.HandleFunc("/health/ready", handler.HealthReady)
	mux.HandleFunc("/internal/validate", handler.ValidateHandler)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("auth-proxy listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("server forced to shutdown: %v", err)
	}

	log.Println("server exited")
}
