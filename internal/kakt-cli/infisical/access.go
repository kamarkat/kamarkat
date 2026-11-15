package infisical

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type AccessOption struct {
	Name     string
	URL      string
	Type     string
	SetupCmd string
}

// AccessInfo is how to REACH this box's Infisical, and deliberately not how to
// authenticate to it.
//
// It carried the administrator's address and password, and its one consumer
// printed both to standard output -- which during a bootstrap is a CI log. The
// credential lives in the box's escrow (ADR-076) and is read from there by
// whoever needs it, so the way to obtain it is a command, not a line of output
// that persists wherever the run was recorded.
type AccessInfo struct {
	Options []AccessOption
	PodName string
}

func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl %v failed: %w\n%s", args, err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func GetAccessInfo(ctx context.Context) *AccessInfo {
	podName, err := GetInfisicalPodName(ctx)
	if err != nil {
		podName = "infisical-standalone-infisical"
	}

	info := &AccessInfo{PodName: podName}

	if u, ok := probeIngress(ctx); ok {
		info.Options = append(info.Options, AccessOption{
			Name: "Ingress (direct browser access)",
			URL:  u,
			Type: "ingress",
		})
	}

	info.Options = append(info.Options, AccessOption{
		Name: "Port-forward (browser)",
		URL:  "http://localhost:8080",
		Type: "port-forward",
		SetupCmd: fmt.Sprintf("kubectl port-forward -n %s svc/infisical-standalone-infisical 8080:%s",
			infisicalNamespace, infisicalPort),
	})

	return info
}

func probeIngress(ctx context.Context) (string, bool) {
	output, err := kubectl("get", "ingress", "-n", infisicalNamespace, "-o", "name")
	if err != nil {
		return "", false
	}
	var ingressName string
	for _, name := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.Contains(strings.ToLower(name), "infisical") {
			ingressName = strings.TrimPrefix(name, "ingress.networking.k8s.io/")
			break
		}
	}
	if ingressName == "" {
		return "", false
	}
	hostOut, err := kubectl("get", "ingress", "-n", infisicalNamespace, ingressName,
		"-o", `jsonpath={.spec.rules[0].host}`)
	if err != nil || hostOut == "" {
		return "", false
	}
	tlsOut, _ := kubectl("get", "ingress", "-n", infisicalNamespace, ingressName,
		"-o", `jsonpath={.spec.tls[0].hosts[0]}`)
	scheme := "http"
	if tlsOut == hostOut {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, hostOut), true
}
