// The Support Agent: a bounded control-plane evidence collector.
//
// ADR-077 defines what this is and, as importantly, what it is not. It holds no
// Kubernetes read access — its ClusterRole grants `create` on tokenreviews and
// subjectaccessreviews and nothing else — and it reaches the platform's
// components over HTTP the way any scraper would. It has no Service, no Ingress
// and no ingress NetworkPolicy rule, so nothing can reach it.
//
// It ships to every box and is inert until enrolled. Enrolment is a client
// certificate existing on disk (ADR-077 addendum 1); there is no token. An
// unenrolled box is not degraded, it simply has nowhere to report, and that is
// the honest meaning of "what is not observed is not supported" (ADR-067).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kamarkat/kamarkat/internal/support"
	"gopkg.in/yaml.v3"
)

func main() {
	var (
		allowlistPath = flag.String("allowlist", "/etc/support-agent/allowlist.yaml",
			"the collection contract (ADR-077)")
		interval = flag.Duration("interval", time.Hour, "how often to collect")
		endpoint = flag.String("endpoint", os.Getenv("SUPPORT_ENDPOINT"),
			"the platform's telemetry ingress")
		certFile = flag.String("cert", "/etc/support-agent/tls/tls.crt", "client certificate")
		keyFile  = flag.String("key", "/etc/support-agent/tls/tls.key", "client key")
		// The SUPPORT PLANE's CA, not the tenant CA cert-manager writes into
		// the agent's own Secret. That one signs this agent and says nothing
		// about who may terminate the other end; validating the platform's
		// server certificate against it fails every time, and the tempting
		// fix is to stop verifying.
		caFile = flag.String("ca", "/etc/support-agent/enrolment/ca.crt",
			"the Support Plane's CA certificate")
		gitopsDir = flag.String("gitops-dir", "", "repository artefact collectors read, if mounted")
		once      = flag.Bool("once", false, "collect and exit")
	)
	flag.Parse()

	log.SetFlags(log.LUTC | log.Ldate | log.Ltime)

	raw, err := os.ReadFile(*allowlistPath)
	if err != nil {
		log.Fatalf("no allowlist at %s: %v.\nThe allowlist is the boundary; an "+
			"agent that ran without one would have no bound on what it gathers",
			*allowlistPath, err)
	}
	// The ConfigMap is mounted by key, so the file IS the document. Parsed
	// strictly: Parse refuses an allowlist that would not bound anything, and a
	// malformed one must not load as a permissive one.
	var doc []byte
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err == nil && cm.Data["allowlist.yaml"] != "" {
		doc = []byte(cm.Data["allowlist.yaml"])
	} else {
		doc = raw
	}
	allow, err := support.Parse(doc)
	if err != nil {
		log.Fatalf("allowlist: %v", err)
	}

	// The tenant's subtraction. Effective scope is `allowlist − denylist`, and
	// nothing here can widen it: Effective only removes.
	var deny []string
	if v := os.Getenv("SUPPORT_DENY"); v != "" {
		for _, f := range splitComma(v) {
			deny = append(deny, f)
		}
	}
	allow = allow.Effective(deny)
	log.Printf("scope: %d collector(s), %d field(s)", len(allow.Collectors), len(allow.Fields()))

	enr := support.Enrolment{
		CertFile: *certFile, KeyFile: *keyFile, CAFile: *caFile, Endpoint: *endpoint,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reader := &support.ClusterReader{GitopsDir: *gitopsDir}

	// Revoked is an operational state of this process, not a state of the
	// enrolment: `REVOKED` is the platform's record. Here it only means "do not
	// collect this cycle", and the long re-probe is what lets a re-enrolled box
	// resume without anyone restarting a pod.
	revoked := false
	const reprobe = 24 * time.Hour
	lastProbe := time.Now()

	run := func() {
		if revoked {
			if time.Since(lastProbe) < reprobe {
				return
			}
			lastProbe = time.Now()
		}
		// Enrolment is re-read every cycle rather than at start-up. A tenant who
		// revokes by deleting the Secret should stop being reported within one
		// interval, without anyone restarting a pod, and a tenant who enrols
		// should start without one either.
		if !enr.Enrolled() {
			log.Printf("not enrolled: no client certificate at %s. Collecting "+
				"nothing and reporting nowhere; the platform is unaffected and "+
				"so is this cluster (ADR-077)", enr.CertFile)
			return
		}
		identity, err := enr.Identity()
		if err != nil {
			log.Printf("enrolled but the certificate will not load: %v", err)
			return
		}

		payload := support.Collect(ctx, allow, reader, identity)
		for name, why := range payload.Skipped {
			log.Printf("not collected: %s: %s", name, why)
		}

		t, err := support.NewTransport(enr)
		if err != nil {
			log.Printf("transport: %v", err)
			return
		}
		if err := t.Send(ctx, payload); err != nil {
			// ADR-077 addendum 3, decision 6. Revocation is the one failure that
			// is not transient, and it is the one the platform states rather
			// than one this agent infers from a status code.
			if errors.Is(err, support.ErrEnrolmentRevoked) {
				revoked = true
				log.Printf("this box's support enrolment has been revoked by the "+
					"platform. Nothing else changes: the cluster, its workloads "+
					"and every capability continue exactly as before, and this "+
					"agent stays installed and inert. If you believe this is "+
					"wrong, contact support -- re-enrolment is picked up without "+
					"restarting anything. Re-probing every %v.", reprobe)
				return
			}
			// Everything else is transient and is retried on the next cycle. A
			// failed send is logged and dropped, never queued: support evidence
			// is a periodic sample of current state, so a missed cycle is
			// answered by the next one, and a spool would turn a support channel
			// into a store of a tenant's control-plane history sitting in their
			// own cluster.
			log.Printf("send: %v (will retry)", err)
			return
		}
		// A successful send clears a previous revocation: the platform has
		// accepted this agent again, which is what re-enrolment looks like from
		// in here.
		revoked = false
		log.Printf("reported %d record(s) as %s", len(payload.Records), identity)
	}

	run()
	if *once {
		return
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("shutting down")
			return
		case <-ticker.C:
			run()
		}
	}
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
