package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"
)

// APIGatewayEvent matches Netlify / AWS Lambda API Gateway proxy request.
type APIGatewayEvent struct {
	Path                  string              `json:"path"`
	HTTPMethod            string              `json:"httpMethod"`
	Headers               map[string]string   `json:"headers"`
	MultiValueHeaders     map[string][]string `json:"multiValueHeaders,omitempty"`
	QueryStringParameters map[string]string   `json:"queryStringParameters,omitempty"`
	Body                  string              `json:"body,omitempty"`
	IsBase64Encoded       bool                `json:"isBase64Encoded,omitempty"`
}

// APIGatewayResponse matches Netlify / AWS Lambda API Gateway proxy response.
type APIGatewayResponse struct {
	StatusCode        int                 `json:"statusCode"`
	Headers           map[string]string   `json:"headers,omitempty"`
	MultiValueHeaders map[string][]string `json:"multiValueHeaders,omitempty"`
	Body              string              `json:"body"`
	IsBase64Encoded   bool                `json:"isBase64Encoded"`
}

// RunLambdaRuntime runs the standard AWS Lambda / Netlify Runtime API loop.
func RunLambdaRuntime(handler http.Handler) {
	runtimeAPI := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if runtimeAPI == "" {
		return
	}

	client := &http.Client{Timeout: 0} // no timeout for long-polling next invocation
	baseURL := fmt.Sprintf("http://%s/2018-06-01/runtime/invocation", runtimeAPI)

	log.Printf("[*] Netlify Go Serverless Proxy Lambda runtime started (API: %s)", runtimeAPI)

	for {
		// 1. Get next invocation
		resp, err := client.Get(fmt.Sprintf("%s/next", baseURL))
		if err != nil {
			log.Printf("[!] Failed to get next Lambda invocation: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		reqID := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			log.Printf("[!] Failed to read invocation body: %v", err)
			continue
		}

		var event APIGatewayEvent
		if err := json.Unmarshal(bodyBytes, &event); err != nil {
			log.Printf("[!] Failed to unmarshal API Gateway event: %v", err)
			continue
		}

		// 2. Convert event to http.Request
		var reqBody io.Reader
		if event.Body != "" {
			if event.IsBase64Encoded {
				decoded, bErr := base64.StdEncoding.DecodeString(event.Body)
				if bErr == nil {
					reqBody = bytes.NewReader(decoded)
				}
			} else {
				reqBody = strings.NewReader(event.Body)
			}
		}

		pathAndQuery := event.Path
		if len(event.QueryStringParameters) > 0 {
			q := url.Values{}
			for k, v := range event.QueryStringParameters {
				q.Set(k, v)
			}
			pathAndQuery += "?" + q.Encode()
		}

		httpReq, err := http.NewRequestWithContext(context.Background(), event.HTTPMethod, pathAndQuery, reqBody)
		if err != nil {
			postLambdaError(client, baseURL, reqID, err.Error())
			continue
		}

		for k, v := range event.Headers {
			httpReq.Header.Set(k, v)
			if strings.EqualFold(k, "host") && httpReq.Host == "" {
				httpReq.Host = v
			}
		}
		for k, values := range event.MultiValueHeaders {
			for _, v := range values {
				httpReq.Header.Add(k, v)
			}
		}

		start := time.Now()
		log.Printf("[lambda] received reqID=%s method=%s path=%s host=%s range=%s",
			reqID, event.HTTPMethod, pathAndQuery, event.Headers["host"], event.Headers["range"])

		// 3. Execute HTTP handler
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httpReq)

		// 4. Build API Gateway response
		lambdaResp := APIGatewayResponse{
			StatusCode: rec.Code,
			Headers:    make(map[string]string),
		}

		for k, values := range rec.Header() {
			if len(values) > 0 {
				lambdaResp.Headers[k] = values[0]
			}
		}

		resBodyBytes := rec.Body.Bytes()
		log.Printf("[lambda] completed reqID=%s status=%d body_bytes=%d duration=%v",
			reqID, rec.Code, len(resBodyBytes), time.Since(start))

		// Always base64 encode response for safe binary/video/chunk transport
		lambdaResp.Body = base64.StdEncoding.EncodeToString(resBodyBytes)
		lambdaResp.IsBase64Encoded = true

		respJSON, _ := json.Marshal(lambdaResp)
		postResp, pErr := client.Post(fmt.Sprintf("%s/%s/response", baseURL, reqID), "application/json", bytes.NewReader(respJSON))
		if pErr == nil {
			postResp.Body.Close()
		} else {
			log.Printf("[!] Failed to post response to Lambda runtime: %v", pErr)
		}
	}
}

func postLambdaError(client *http.Client, baseURL, reqID, errMsg string) {
	errPayload, _ := json.Marshal(map[string]string{"errorMessage": errMsg})
	resp, err := client.Post(fmt.Sprintf("%s/%s/error", baseURL, reqID), "application/json", bytes.NewReader(errPayload))
	if err == nil {
		resp.Body.Close()
	}
}
