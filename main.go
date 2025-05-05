// Argon-Proxy: A CORS proxy server that handles URL query parameters properly
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

// Command line flags
var (
	port          = flag.Int("port", 8080, "Port to listen on")
	address       = flag.String("address", "127.0.0.1", "Address to listen on")
	allowedOrigin = flag.String("allow-origin", "*", "CORS Allow-Origin header value")
	verbose       = flag.Bool("verbose", false, "Enable verbose logging")
	trustProxy    = flag.Bool("trust-proxy", false, "Trust X-Forwarded-* headers from Nginx")
)

//go:embed getconfig/*
var SampleConfigs embed.FS

// main is the entry point for the CORS proxy server
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
	log.Printf("Server starting on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// -----------------------------
// PROXY REQUEST HANDLING
// -----------------------------

// handleProxy processes proxy requests to external services
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

// parseTargetURL extracts the target URL from the request
// Uses manual parsing to handle nested query parameters
func parseTargetURL(r *http.Request) string {
	// First try to get the target from query parameters
	rawQuery := r.URL.RawQuery
	targetPrefix := "target="
	targetStart := strings.Index(rawQuery, targetPrefix)

	if targetStart == -1 {
		// If not in query, check if target is provided in the path
		if strings.HasPrefix(r.URL.Path, "/proxy/") {
			return r.URL.Path[len("/proxy/"):]
		}
		return ""
	}

	// Extract target value from raw query string
	targetValueEncoded := rawQuery[targetStart+len(targetPrefix):]
	endIndex := strings.Index(targetValueEncoded, "&")
	if endIndex != -1 {
		targetValueEncoded = targetValueEncoded[:endIndex]
	}

	if *verbose {
		log.Printf("Target URL from raw query (encoded): %s", targetValueEncoded)
	}

	return targetValueEncoded
}

// processProxyRequest handles the proxy forwarding logic
func processProxyRequest(w http.ResponseWriter, r *http.Request, rawTargetURL string) {
	if *verbose {
		log.Printf("Processing raw target URL: %s", rawTargetURL)
	}

	// Decode the raw target URL string
	decodedURL, err := url.QueryUnescape(rawTargetURL)
	if err != nil {
		http.Error(w, "Invalid URL encoding in target", http.StatusBadRequest)
		return
	}

	// Ensure the URL has a scheme (http:// or https://)
	if !strings.HasPrefix(decodedURL, "http://") && !strings.HasPrefix(decodedURL, "https://") {
		decodedURL = "https://" + decodedURL
	}

	if *verbose {
		log.Printf("Decoded target URL: %s", decodedURL)
	}

	// Process additional query parameters
	finalURL := buildFinalURL(r, decodedURL)

	// Create proxy request
	proxyReq, err := createProxyRequest(r, finalURL)
	if err != nil {
		http.Error(w, "Error creating proxy request", http.StatusInternalServerError)
		return
	}

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

// buildFinalURL constructs the final URL with additional parameters
func buildFinalURL(r *http.Request, decodedURL string) string {
	// Extract non-target query parameters
	rawQuery := r.URL.RawQuery
	additionalParams := ""
	for _, part := range strings.Split(rawQuery, "&") {
		if !strings.HasPrefix(part, "target=") && part != "" {
			if additionalParams == "" {
				additionalParams = part
			} else {
				additionalParams += "&" + part
			}
		}
	}

	// Combine target URL with additional parameters
	finalURL := decodedURL
	if additionalParams != "" {
		if strings.Contains(decodedURL, "?") {
			finalURL += "&" + additionalParams
		} else {
			finalURL += "?" + additionalParams
		}
	}

	if *verbose {
		log.Printf("Final URL to proxy: %s", finalURL)
	}

	return finalURL
}

// createProxyRequest creates a new HTTP request for the target URL
func createProxyRequest(r *http.Request, finalURL string) (*http.Request, error) {
	proxyReq, err := http.NewRequest(r.Method, finalURL, r.Body)
	if err != nil {
		return nil, err
	}

	// Copy original headers
	copyRequestHeaders(r, proxyReq)

	// Set the Host header from the target URL
	if hostStart := strings.Index(finalURL, "://"); hostStart != -1 {
		hostPort := finalURL[hostStart+3:]
		slash := strings.Index(hostPort, "/")
		if slash != -1 {
			hostPort = hostPort[:slash]
		}
		proxyReq.Host = hostPort
	}

	return proxyReq, nil
}

// copyRequestHeaders copies relevant headers from the original request
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

// processProxyResponse handles the response from the target server
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

// -----------------------------
// CORS HANDLING
// -----------------------------

// handlePreflight handles CORS preflight OPTIONS requests
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

// addCORSHeaders adds CORS headers to the response
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

// -----------------------------
// UTILITY FUNCTIONS
// -----------------------------

// getClientIP extracts the original client IP address
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

// shouldSkipHeader returns true if a header should not be forwarded
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

// -----------------------------
// CONFIG FILE HANDLING
// -----------------------------

// handleConfigFiles serves embedded configuration files
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

// getContentType determines content type based on file extension
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

// listConfigFiles shows a list of available config files
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

// -----------------------------
// USAGE/HELP FUNCTIONS
// -----------------------------

// handleRoot provides basic usage information
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	displayUsage(w, r, "all")
}

// displayUsage outputs API usage instructions
func displayUsage(w http.ResponseWriter, r *http.Request, section string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)

	if *verbose {
		log.Printf("Original request: %s", r.URL.RawQuery)
	}

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

// printStartupInfo logs information about the server configuration
func printStartupInfo(listenAddr string) {
	log.Printf("Starting CORS proxy server on %s", listenAddr)
	log.Printf("CORS proxy supports:")
	log.Printf("  - http://%s/proxy/{target-url}", listenAddr)
	log.Printf("  - http://%s/proxy/?target={target-url}", listenAddr)
	log.Printf("  - http://%s/getconfig/{filename}", listenAddr)
	log.Printf("CORS Allow-Origin: %s", *allowedOrigin)
	log.Printf("Trust X-Forwarded-* headers: %v", *trustProxy)
}
