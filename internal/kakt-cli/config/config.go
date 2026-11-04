package config

import (
	"os"
	"strconv"
	
	"github.com/kamarkat/kamarkat/internal/kakt-cli/constants"
)

type Config struct {
	MonitoringNamespace string
	EnableEventEmission bool
	TopologyLabelPrefix string
}

func LoadConfigFromEnv() Config {
	return Config{
		MonitoringNamespace: getEnvOrDefault("MONITORING_NAMESPACE", constants.NamespaceCAPI),
		EnableEventEmission: parseBoolEnv("ENABLE_EVENT_EMISSION", true),
		TopologyLabelPrefix: getEnvOrDefault("TOPOLOGY_LABEL_PREFIX", "nutgraf.in/"),
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func parseBoolEnv(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return defaultValue
		}
		return parsed
	}
	return defaultValue
}
