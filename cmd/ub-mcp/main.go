package main // FCIS: Imperative Shell

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sysop/ultrabridge/internal/mcpauth"
	"github.com/sysop/ultrabridge/internal/notedb"
)

func main() {
	httpAddr := flag.String("http", "", "HTTP SSE address (e.g., :8081). If empty, uses stdio transport.")
	flag.Parse()

	// Open shared notedb for bearer token validation (if configured).
	dbPath := os.Getenv("UB_DB_PATH")
	var db *sql.DB
	if dbPath != "" {
		var err error
		db, err = notedb.Open(context.Background(), dbPath)
		if err != nil {
			log.Fatalf("notedb open: %v", err)
		}
		defer db.Close()
		if err := mcpauth.Migrate(context.Background(), db); err != nil {
			log.Fatalf("mcpauth migrate: %v", err)
		}
	}

	apiURL := os.Getenv("UB_MCP_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8443"
	}
	apiToken := os.Getenv("UB_MCP_API_TOKEN")
	apiUser := os.Getenv("UB_MCP_API_USER")
	apiPass := os.Getenv("UB_MCP_API_PASS")

	client := newAPIClient(apiURL, apiToken, apiUser, apiPass)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "ultrabridge-notes",
		Version: "1.0.0",
	}, nil)

	registerTools(server, client)

	if *httpAddr != "" {
		// HTTP SSE transport
		mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return server
		}, nil)

		var handler http.Handler = mcpHandler
		staticToken := os.Getenv("UB_MCP_AUTH_TOKEN")

		if db != nil || staticToken != "" || (apiUser != "" && apiPass != "") {
			handler = authMiddleware(db, staticToken, apiUser, apiPass, mcpHandler)
			log.Printf("ub-mcp listening on %s (HTTP SSE, auth enabled)", *httpAddr)
		} else {
			log.Printf("ub-mcp listening on %s (HTTP SSE, WARNING: no auth configured — set UB_DB_PATH or UB_MCP_AUTH_TOKEN)", *httpAddr)
		}
		if err := http.ListenAndServe(*httpAddr, handler); err != nil {
			log.Fatalf("HTTP server failed: %v", err)
		}
	} else {
		// stdio transport (default)
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			log.Fatalf("stdio server failed: %v", err)
		}
	}
}

// apiClient is an HTTP client for calling UltraBridge API endpoints.
type apiClient struct {
	baseURL  string
	token    string
	user     string
	pass     string
	http     *http.Client
}

// newAPIClient creates a new API client. When token is non-empty it is sent as
// a Bearer token (a DB-backed MCP token validated by UltraBridge's auth
// middleware); otherwise the client falls back to Basic Auth with user/pass.
func newAPIClient(baseURL, token, user, pass string) *apiClient {
	return &apiClient{
		baseURL: baseURL,
		token:   token,
		user:    user,
		pass:    pass,
		http:    &http.Client{},
	}
}

// authMiddleware implements the auth chain: DB token → static token → Basic Auth.
func authMiddleware(db *sql.DB, staticToken, basicUser, basicPass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")

		// 1. Bearer token path
		if strings.HasPrefix(auth, "Bearer ") {
			token := auth[len("Bearer "):]

			// 1a. DB-backed token validation
			if db != nil {
				if _, err := mcpauth.ValidateToken(r.Context(), db, token); err == nil {
					next.ServeHTTP(w, r)
					return
				}
			}

			// 1b. Static token fallback (deprecated)
			if staticToken != "" && token == staticToken {
				next.ServeHTTP(w, r)
				return
			}
		}

		// 2. Basic Auth fallback
		if basicUser != "" {
			user, pass, ok := r.BasicAuth()
			if ok && user == basicUser && pass == basicPass {
				next.ServeHTTP(w, r)
				return
			}
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// get performs a GET request to the UltraBridge API with Basic Auth.
func (c *apiClient) get(ctx context.Context, path string) (*http.Response, error) {
	return c.request(ctx, "GET", path, nil)
}

// postJSON performs a POST with a JSON-encoded body. body may be nil to
// POST without a payload (e.g. side-effect endpoints like purge-completed).
func (c *apiClient) postJSON(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	return c.request(ctx, "POST", path, body)
}

// patchJSON performs a PATCH with a JSON-encoded body. Used for partial
// task updates.
func (c *apiClient) patchJSON(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	return c.request(ctx, "PATCH", path, body)
}

// deleteRequest performs a DELETE request. No body.
func (c *apiClient) deleteRequest(ctx context.Context, path string) (*http.Response, error) {
	return c.request(ctx, "DELETE", path, nil)
}

// request is the shared path for every method. Marshals body as JSON when
// non-nil; sets Content-Type for bodied requests.
func (c *apiClient) request(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	return c.http.Do(req)
}
