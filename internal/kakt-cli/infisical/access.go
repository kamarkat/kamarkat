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

type AccessInfo struct {
	Options    []AccessOption
	AdminEmail string
	Password   string
	PodName    string
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

	info := &AccessInfo{
		AdminEmail: adminEmail(),
		Password:   adminPassword(),
		PodName:    podName,
	}

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
