package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
)

var (
	port          = flag.Int("port", 8080, "Port to listen on")
	address       = flag.String("address", "127.0.0.1", "Address to listen on")
	allowedOrigin = flag.String("allow-origin", "*", "CORS Allow-Origin header value")
	verbose       = flag.Bool("verbose", false, "Enable verbose logging")
	trustProxy    = flag.Bool("trust-proxy", false, "Trust X-Forwarded-* headers from Nginx")
)

//go:embed getconfig/*
var SampleConfigs embed.FS

func main() {
	flag.Parse()

	// Register HTTP handlers
	http.HandleFunc("/proxy/", handleProxy)
	http.HandleFunc("/proxy", handleProxy) // Also handle /proxy without trailing slash
	http.HandleFunc("/getconfig/", handleConfigFiles)
	http.HandleFunc("/", handleRoot)

	// Format listen address
	listenAddr := fmt.Sprintf("%s:%d", *address, *port)

	// Log startup information
	printStartupInfo(listenAddr)

	// Start the server
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// Print startup information
func printStartupInfo(listenAddr string) {
	log.Printf("Starting CORS proxy server on %s", listenAddr)
	log.Printf("CORS proxy supports:")
	log.Printf("  - http://%s/proxy/{target-url}", listenAddr)
	log.Printf("  - http://%s/proxy/?target={target-url}", listenAddr)
	log.Printf("  - http://%s/getconfig/{filename}", listenAddr)
	log.Printf("CORS Allow-Origin: %s", *allowedOrigin)
	log.Printf("Trust X-Forwarded-* headers: %v", *trustProxy)
}

// displayUsage outputs API usage instructions to the response writer
func displayUsage(w http.ResponseWriter, r *http.Request, section string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, "CORS Proxy Usage:\n")

	// Show general usage info
	fmt.Fprintf(w, "GET /proxy/{url} - Proxy to the specified URL\n")
	fmt.Fprintf(w, "GET /getconfig/{filename} - Get embedded configuration file\n")

	// Show section-specific examples
	if section == "proxy" || section == "all" {
		fmt.Fprintf(w, "\nProxy Examples:\n")
		fmt.Fprintf(w, "  - GET /proxy/https://api.example.com/data\n")
		fmt.Fprintf(w, "  - GET /proxy/?target=https://api.example.com/data\n")
	}

	if section == "config" || section == "all" {
		fmt.Fprintf(w, "\nConfig Examples:\n")
		fmt.Fprintf(w, "  - GET /getconfig/nginx\n")
	}
}

// Root handler to provide usage information
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	displayUsage(w, r, "all")
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	// Handle OPTIONS requests for CORS preflight
	if r.Method == "OPTIONS" {
		handlePreflight(w, r)
		return
	}

	// Parse target URL from request
	targetURL := parseTargetURL(r)

	if targetURL == "" {
		displayUsage(w, r, "proxy")
		return
	}

	// Process the proxy request
	processProxyRequest(w, r, targetURL)
}

// Parse target URL from request
func parseTargetURL(r *http.Request) string {
	// Check if target is provided as a query parameter
	targetParam := r.URL.Query().Get("target")
	if targetParam != "" {
		if *verbose {
			log.Printf("Target URL from query parameter: %s", targetParam)
		}
		return targetParam
	}

	// Target provided in the path
	if strings.HasPrefix(r.URL.Path, "/proxy/") {
		return r.URL.Path[len("/proxy/"):]
	}

	// No target URL found
	return ""
}

// Process the proxy request with the given target URL
func processProxyRequest(w http.ResponseWriter, r *http.Request, targetURL string) {
	// Decode the URL if it's URL-encoded
	decodedURL, err := url.QueryUnescape(targetURL)
	if err != nil {
		http.Error(w, "Invalid URL encoding", http.StatusBadRequest)
		return
	}

	// Ensure the URL has a scheme (http:// or https://)
	if !strings.HasPrefix(decodedURL, "http://") && !strings.HasPrefix(decodedURL, "https://") {
		decodedURL = "https://" + decodedURL
	}

	if *verbose {
		log.Printf("Proxying request to: %s", decodedURL)
		if *trustProxy {
			log.Printf("Original client IP: %s", getClientIP(r))
		}
	}

	// Parse the target URL
	parsedURL, err := url.Parse(decodedURL)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	// Copy original query parameters excluding 'target'
	copyQueryParams(r, parsedURL)

	// Create and execute the proxy request
	executeProxyRequest(w, r, parsedURL)
}

// Copy query parameters from original request to target URL
func copyQueryParams(r *http.Request, targetURL *url.URL) {
	if r.URL.RawQuery != "" {
		values := r.URL.Query()
		values.Del("target") // Remove the target parameter

		if len(values) > 0 {
			// If there are other query parameters, append them
			if targetURL.RawQuery != "" {
				targetURL.RawQuery += "&" + values.Encode()
			} else {
				targetURL.RawQuery = values.Encode()
			}
		}
	}
}

// Create and execute the proxy request
func executeProxyRequest(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
	// Create a new request
	proxyReq, err := http.NewRequest(r.Method, targetURL.String(), r.Body)
	if err != nil {
		http.Error(w, "Error creating proxy request", http.StatusInternalServerError)
		return
	}

	// Copy original headers
	copyRequestHeaders(r, proxyReq)

	// Set the Host header
	proxyReq.Host = targetURL.Host

	// Send the request
	client := &http.Client{}
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error proxying request: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Process the response
	processProxyResponse(w, r, resp)
}

// Copy request headers to proxy request
func copyRequestHeaders(r *http.Request, proxyReq *http.Request) {
	// Copy original headers, except those that should be skipped
	for key, values := range r.Header {
		if !shouldSkipHeader(key) {
			for _, value := range values {
				proxyReq.Header.Add(key, value)
			}
		}
	}

	// Forward the real client IP if available
	if *trustProxy && r.Header.Get("X-Forwarded-For") != "" {
		proxyReq.Header.Set("X-Real-IP", getClientIP(r))
	}
}

// Process the proxy response
func processProxyResponse(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	// Add CORS headers
	addCORSHeaders(w, r)

	// Copy the response headers, excluding ones that might conflict with our CORS headers
	for key, values := range resp.Header {
		if !strings.HasPrefix(strings.ToLower(key), "access-control-") {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
	}

	// Set the status code
	w.WriteHeader(resp.StatusCode)

	// Copy the response body
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("Error copying response: %v", err)
	}
}

// Handle preflight OPTIONS requests
func handlePreflight(w http.ResponseWriter, r *http.Request) {
	addCORSHeaders(w, r)

	// Handle the specific Access-Control-Request-Method header
	if r.Header.Get("Access-Control-Request-Method") != "" {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE, HEAD, PATCH")
	}

	// Handle the specific Access-Control-Request-Headers header
	if r.Header.Get("Access-Control-Request-Headers") != "" {
		requestHeaders := r.Header.Get("Access-Control-Request-Headers")
		w.Header().Set("Access-Control-Allow-Headers", requestHeaders)
	} else {
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
	}

	// Set max age for preflight cache
	w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours

	w.WriteHeader(http.StatusNoContent) // 204 No Content
}

// Add CORS headers to the response
func addCORSHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")

	// If the request has an Origin header and it's allowed, use it for CORS
	if origin != "" && (*allowedOrigin == "*" || *allowedOrigin == origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	} else {
		w.Header().Set("Access-Control-Allow-Origin", *allowedOrigin)
	}

	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE, HEAD, PATCH")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Vary", "Origin")
}

// Get the real client IP from headers set by Nginx
func getClientIP(r *http.Request) string {
	// X-Forwarded-For can be comma-separated list of IPs
	// The leftmost IP is the original client IP
	if *trustProxy {
		xForwardedFor := r.Header.Get("X-Forwarded-For")
		if xForwardedFor != "" {
			// Get the first IP in the list
			ips := strings.Split(xForwardedFor, ",")
			return strings.TrimSpace(ips[0])
		}

		// If X-Real-IP header is set (common in Nginx)
		if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
			return realIP
		}
	}

	// Fall back to RemoteAddr if we don't have forwarded headers
	return strings.Split(r.RemoteAddr, ":")[0]
}

// Check if we should skip forwarding this header
func shouldSkipHeader(key string) bool {
	lower := strings.ToLower(key)
	return strings.EqualFold(key, "Connection") ||
		strings.EqualFold(key, "Host") ||
		strings.EqualFold(key, "X-Forwarded-Host") ||
		strings.EqualFold(key, "X-Forwarded-Proto") ||
		strings.EqualFold(key, "Content-Length") ||
		// Skip Nginx specific headers that should not be forwarded
		strings.HasPrefix(lower, "x-nginx")
}

// Handle serving configuration files from embedded filesystem
func handleConfigFiles(w http.ResponseWriter, r *http.Request) {
	// Extract the filename from the path
	filename := strings.TrimPrefix(r.URL.Path, "/getconfig/")

	if filename == "" {
		// If no specific file requested, show available configs
		listConfigFiles(w, r)
		return
	}

	// Construct the full path within the embedded filesystem
	filePath := path.Join("getconfig", filename)

	if *verbose {
		log.Printf("Attempting to serve config file: %s", filePath)
	}

	// Try to read the file from embedded filesystem
	content, err := SampleConfigs.ReadFile(filePath)
	if err != nil {
		if *verbose {
			log.Printf("Error reading config file: %v", err)
		}
		http.Error(w, "Configuration file not found", http.StatusNotFound)
		return
	}

	// Add CORS headers
	addCORSHeaders(w, r)

	// Set content type based on file extension
	contentType := getContentType(filename)
	w.Header().Set("Content-Type", contentType)

	// Write the file content to response
	w.Write(content)

	if *verbose {
		log.Printf("Successfully served config file: %s", filePath)
	}
}

// Get content type based on file extension
func getContentType(filename string) string {
	switch path.Ext(filename) {
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".conf":
		return "text/plain"
	default:
		return "text/plain"
	}
}

// List available configuration files
func listConfigFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")

	fmt.Fprintf(w, "Available configuration files:\n\n")

	fs.WalkDir(SampleConfigs, "getconfig", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			filename := strings.TrimPrefix(path, "getconfig/")
			fmt.Fprintf(w, "- %s\n", filename)
		}
		return nil
	})

	fmt.Fprintf(w, "\nUsage: GET /getconfig/{filename}\n")
}
