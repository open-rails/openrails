package hostconfig

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type servicePort struct {
	name, proto string
	port        int
}

// kubeletServiceEnv is what the kubelet injects for one Service
// (pkg/kubelet/envvars.FromServices).
func kubeletServiceEnv(service, clusterIP string, ports ...servicePort) map[string]string {
	prefix := strings.ToUpper(strings.ReplaceAll(service, "-", "_"))
	env := map[string]string{
		prefix + "_SERVICE_HOST": clusterIP,
		prefix + "_SERVICE_PORT": fmt.Sprint(ports[0].port),
	}
	for i, p := range ports {
		if p.name != "" {
			env[prefix+"_SERVICE_PORT_"+strings.ToUpper(strings.ReplaceAll(p.name, "-", "_"))] = fmt.Sprint(p.port)
		}
		proto := strings.ToLower(p.proto)
		url := proto + "://" + net.JoinHostPort(clusterIP, fmt.Sprint(p.port))
		if i == 0 {
			env[prefix+"_PORT"] = url
		}
		at := fmt.Sprintf("%s_PORT_%d_%s", prefix, p.port, strings.ToUpper(proto))
		env[at] = url
		env[at+"_PROTO"] = proto
		env[at+"_PORT"] = fmt.Sprint(p.port)
		env[at+"_ADDR"] = clusterIP
	}
	return env
}

// A pod sees every Service in its namespace; one named like a config section
// must not refuse boot, and the section's real settings still load.
func TestLoadIgnoresKubernetesServiceLinks(t *testing.T) {
	bootEnv(t)
	// The variables of the field report: a Service named redis.
	env := map[string]string{
		"REDIS_SERVICE_HOST":        "10.96.14.7",
		"REDIS_SERVICE_PORT":        "6379",
		"REDIS_SERVICE_PORT_REDIS":  "6379",
		"REDIS_PORT":                "tcp://10.96.14.7:6379",
		"REDIS_PORT_6379_TCP":       "tcp://10.96.14.7:6379",
		"REDIS_PORT_6379_TCP_PROTO": "tcp",
		"REDIS_PORT_6379_TCP_PORT":  "6379",
		"REDIS_PORT_6379_TCP_ADDR":  "10.96.14.7",
	}
	for _, svc := range []map[string]string{
		kubeletServiceEnv("db", "10.96.0.12", servicePort{"postgres", "TCP", 5432}),
		kubeletServiceEnv("db-rw", "10.96.0.13", servicePort{"postgres", "TCP", 5432}),
		kubeletServiceEnv("vault", "10.96.3.4", servicePort{"http", "TCP", 8200}, servicePort{"https-internal", "TCP", 8201}),
		kubeletServiceEnv("auth", "10.96.5.6", servicePort{"", "TCP", 80}),
		kubeletServiceEnv("logger", "10.96.7.8", servicePort{"syslog", "UDP", 514}, servicePort{"otlp", "TCP", 4317}),
		kubeletServiceEnv("llm", "fd00:10:96::a", servicePort{"http", "TCP", 8080}),
		kubeletServiceEnv("private", "10.96.9.9", servicePort{"", "TCP", 9090}),
		kubeletServiceEnv("hyperswitch", "10.96.9.10", servicePort{"", "SCTP", 8080}),
		// Services named like retired config families.
		kubeletServiceEnv("store", "10.96.9.11", servicePort{"", "TCP", 80}),
		kubeletServiceEnv("clickhouse", "10.96.9.12", servicePort{"http", "TCP", 8123}),
	} {
		for k, v := range svc {
			env[k] = v
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("DB_URL", "postgres://u:p@db:5432/openrails?sslmode=disable")

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "redis:6379", cfg.Redis.Addr)
	require.Equal(t, "postgres://u:p@db:5432/openrails?sslmode=disable", cfg.DB.URL)
	require.Nil(t, cfg.Vault, "a Service named vault declares no Vault")
	require.Nil(t, cfg.LLM)
	require.Zero(t, cfg.PrivatePort)

	// The same names holding real settings are config.
	t.Setenv("DB_PORT", "6543")
	t.Setenv("PRIVATE_PORT", "9091")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, "6543", cfg.DB.Port)
	require.Equal(t, 9091, cfg.PrivatePort)

	// Anything else in a section is still refused.
	for key, value := range map[string]string{
		"REDIS_TYPO":               "1",
		"REDIS_PORT":               "6379",
		"REDIS_SERVICE_HOST":       "redis.internal",
		"REDIS_PORT_6379_TCP":      "tcp://10.96.14.7:6380",
		"REDIS_PORT_6379_TCP_PORT": "6380",
	} {
		t.Run("refuses "+key+"="+value, func(t *testing.T) {
			t.Setenv(key, value)
			_, err := Load("")
			require.ErrorContains(t, err, "invalid keys")
		})
	}
	t.Run("refuses a real retired setting", func(t *testing.T) {
		t.Setenv("STORE_NAME", "Acme")
		_, err := Load("")
		require.ErrorContains(t, err, "store config was removed")
	})
}
