package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"netlify-proxy-go/pkg/proxy"
)

func main() {
	secret := proxy.EnvOrDefault("PROXY_SECRET", proxy.EnvOrDefault("WORKER_SECRET", ""))
	allowedDomains := os.Getenv("ALLOWED_UPSTREAM_DOMAINS")
	allowInsecure := os.Getenv("ALLOW_INSECURE_TEST_UPSTREAM") == "true"

	handler := proxy.NewProxyHandler(secret, allowedDomains, allowInsecure)

	// If running inside AWS Lambda / Netlify serverless environment
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		proxy.RunLambdaRuntime(handler)
		return
	}

	// Standalone local development server
	port := proxy.EnvOrDefault("PORT", "8888")
	host := proxy.EnvOrDefault("HOST", "0.0.0.0")
	addr := fmt.Sprintf("%s:%s", host, port)

	log.Printf("[*] Netlify Go Serverless Proxy listening on http://%s", addr)
	log.Printf("[*] Health check: http://127.0.0.1:%s/health", port)
	log.Printf("[*] Streamtape extract: http://127.0.0.1:%s/extract/st/0A2vDYQz3wIbPQ6", port)

	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
