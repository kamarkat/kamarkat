package main

import (
	"fmt"

	"github.com/kamarkat/kamarkat/internal/kakt-cli/infisical"
)

func printStepBanner(section, title, goal, duration string) {
	fmt.Println()
	fmt.Println("──────────────────────────────────────────────────────────────────────")
	fmt.Printf("  Step %s: %s\n", section, title)
	fmt.Printf("  Goal: %s\n", goal)
	if duration != "" {
		fmt.Printf("  Duration: %s\n", duration)
	}
	fmt.Println("──────────────────────────────────────────────────────────────────────")
}

func printManualActions(info *infisical.AccessInfo) {
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("  Infisical Platform — Access Information")
	fmt.Println("======================================================================")
	fmt.Println()

	fmt.Println("  Dashboard:")
	fmt.Println()
	for i, opt := range info.Options {
		fmt.Printf("    %d. %s\n", i+1, opt.Name)
		fmt.Printf("       %s\n", opt.URL)
		if opt.SetupCmd != "" {
			fmt.Printf("       $ %s\n", opt.SetupCmd)
		}
	}

	fmt.Println()
	fmt.Println("  Credentials:")
	fmt.Printf("    Email:    %s\n", info.AdminEmail)
	fmt.Printf("    Password: %s\n", info.Password)
	fmt.Println()

	fmt.Println("  Bootstrap summary:")
	fmt.Println("    • Organization: Zero-Ops (auto-created)")
	fmt.Println("    • Project (certs): hub-platform (auto-created, type: cert-manager)")
	fmt.Println("    • Project (secrets): hub-secrets (auto-created, type: secret-manager)")
	fmt.Println("    • Machine Identity: hub-platform-eso (auto-created, Universal Auth)")
	fmt.Println("    • Certificate Profile: argocd-bootstrap (auto-created, CA: Fleet Intermediate CA)")
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println()
}
