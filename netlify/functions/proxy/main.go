package main

import (
	"os"

	"netlify-proxy-go/pkg/proxy"
)

func main() {
	secret := proxy.EnvOrDefault("PROXY_SECRET", proxy.EnvOrDefault("WORKER_SECRET", ""))
	allowedDomains := os.Getenv("ALLOWED_UPSTREAM_DOMAINS")
	allowInsecure := os.Getenv("ALLOW_INSECURE_TEST_UPSTREAM") == "true"

	handler := proxy.NewProxyHandler(secret, allowedDomains, allowInsecure)

	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		proxy.RunLambdaRuntime(handler)
		return
	}

	// Local fallback if run directly
	_ = handler
}
