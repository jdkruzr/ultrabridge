package supernote

// pattern: Imperative Shell

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Client is an HTTP client for the Supernote Private Cloud REST API.
type Client struct {
	apiURL   string
	account  string
	password string
	logger   *slog.Logger
	client   http.Client

	mu    sync.Mutex
	token string
}

// NewClient creates a new SPC REST API client.
func NewClient(apiURL, account, password string, logger *slog.Logger) *Client {
	return &Client{
		apiURL:   apiURL,
		account:  account,
		password: password,
		logger:   logger,
		client:   http.Client{Timeout: 30 * time.Second},
	}
}

// Login performs the challenge-response JWT authentication flow using the web UI endpoint.
func (c *Client) Login(ctx context.Context) error {
	// Step 1: Get random code (requires account)
	codeBody := map[string]any{
		"countryCode": nil,
		"account":     c.account,
	}
	var codeResp struct {
		Success    bool   `json:"success"`
		RandomCode string `json:"randomCode"`
		Timestamp  int64  `json:"timestamp"`
	}
	if err := c.postJSON(ctx, "/api/official/user/query/random/code", codeBody, &codeResp, false); err != nil {
		return fmt.Errorf("get random code: %w", err)
	}
	if !codeResp.Success {
		return fmt.Errorf("get random code: SPC returned success=false")
	}

	// Step 2: Hash password with challenge.
	// SPC stores MD5(password). The challenge-response is SHA256(MD5(password) + randomCode).
	md5pw := md5.Sum([]byte(c.password))
	md5hex := fmt.Sprintf("%x", md5pw)
	hash := sha256.Sum256([]byte(md5hex + codeResp.RandomCode))
	hashedPW := fmt.Sprintf("%x", hash)

	// Step 3: Login via web UI endpoint (doesn't displace device session)
	loginBody := map[string]any{
		"countryCode": nil,
		"account":     c.account,
		"password":    hashedPW,
		"browser":     "UltraBridge",
		"equipment":   "1",
		"loginMethod": "1",
		"timestamp":   codeResp.Timestamp,
		"language":    "en",
	}
	var loginResp struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	if err := c.postJSON(ctx, "/api/official/user/account/login/new", loginBody, &loginResp, false); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if !loginResp.Success || loginResp.Token == "" {
		return fmt.Errorf("login: SPC returned success=%v (wrong password or account?)", loginResp.Success)
	}

	c.mu.Lock()
	c.token = loginResp.Token
	c.mu.Unlock()

	c.logger.Info("SPC login successful")
	return nil
}

// FetchTasks returns all tasks from SPC, walking pages until exhausted.
// SPC's /task/all paginates with default maxResults=20 ordered by last_modified
// ASC; the response carries nextPageToken (singular) when more rows exist, while
// requests submit nextPageTokens (plural) — preserve the asymmetry. Earlier
// versions of this client posted {} once, which silently capped UB at 20 rows
// and produced what looked like missing remote tasks.
func (c *Client) FetchTasks(ctx context.Context) ([]SPCTask, error) {
	const maxPages = 1000 // ~20k tasks at default page size; protective cap
	var all []SPCTask
	pageToken := ""
	for pages := 0; pages < maxPages; pages++ {
		var resp struct {
			Success       bool      `json:"success"`
			ErrorMsg      string    `json:"errorMsg"`
			Code          string    `json:"code"`
			ScheduleTask  []SPCTask `json:"scheduleTask"`
			NextPageToken string    `json:"nextPageToken"`
		}
		body := map[string]any{}
		if pageToken != "" {
			body["nextPageTokens"] = pageToken
		}
		if err := c.postJSON(ctx, "/api/file/schedule/task/all", body, &resp, true); err != nil {
			return nil, fmt.Errorf("fetch tasks: %w", err)
		}
		if !resp.Success {
			return nil, fmt.Errorf("fetch tasks: SPC rejected (code=%q msg=%q)", resp.Code, resp.ErrorMsg)
		}
		all = append(all, resp.ScheduleTask...)
		if resp.NextPageToken == "" {
			return all, nil
		}
		pageToken = resp.NextPageToken
	}
	return nil, fmt.Errorf("fetch tasks: exceeded %d pages without termination", maxPages)
}

// CreateTask creates a single task on SPC. Returns an error if SPC reports
// success=false in the response body — historically that signal was ignored,
// which produced phantom sync_map entries with no corresponding remote task.
func (c *Client) CreateTask(ctx context.Context, task SPCTask) error {
	var resp struct {
		Success  bool   `json:"success"`
		ErrorMsg string `json:"errorMsg"`
		Code     string `json:"code"`
	}
	if err := c.postJSON(ctx, "/api/file/schedule/task", task, &resp, true); err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("SPC rejected create (code=%q msg=%q)", resp.Code, resp.ErrorMsg)
	}
	return nil
}

// UpdateTasks performs a bulk update of tasks on SPC.
func (c *Client) UpdateTasks(ctx context.Context, tasks []SPCTask) error {
	body := map[string]any{"updateScheduleTaskList": tasks}
	var resp struct {
		Success  bool   `json:"success"`
		ErrorMsg string `json:"errorMsg"`
		Code     string `json:"code"`
	}
	if err := c.doRequest(ctx, "PUT", "/api/file/schedule/task/list", body, &resp, true, false); err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("SPC rejected update (code=%q msg=%q)", resp.Code, resp.ErrorMsg)
	}
	return nil
}

// DeleteTask deletes a task on SPC by ID. SPC's DELETE response body shape
// hasn't been fully reverse-engineered — sometimes empty, sometimes the
// success envelope. We rely on the HTTP status check inside doRequest;
// passing nil as the result keeps Decode from erroring on an empty body.
func (c *Client) DeleteTask(ctx context.Context, taskID string) error {
	path := "/api/file/schedule/task/" + taskID
	return c.doRequest(ctx, "DELETE", path, nil, nil, true, false)
}

func (c *Client) postJSON(ctx context.Context, path string, body any, result any, auth bool) error {
	return c.doRequest(ctx, "POST", path, body, result, auth, false)
}

// doRequest is the unified HTTP method for all SPC API calls.
func (c *Client) doRequest(ctx context.Context, method, path string, body any, result any, auth, retried bool) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		c.mu.Lock()
		token := c.token
		c.mu.Unlock()
		req.Header.Set("x-access-token", token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized && auth && !retried {
		if err := c.Login(ctx); err != nil {
			return fmt.Errorf("re-auth failed: %w", err)
		}
		return c.doRequest(ctx, method, path, body, result, auth, true)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("SPC %s %s returned %d: %s", method, path, resp.StatusCode, errBody)
	}

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// SPCTask is the wire format for tasks in the SPC REST API.
type SPCTask struct {
	ID            string `json:"taskId"`
	TaskListID    string `json:"taskListId,omitempty"`
	Title         string `json:"title"`
	Detail        string `json:"detail,omitempty"`
	Status        string `json:"status"`
	Importance    string `json:"importance,omitempty"`
	DueTime       int64  `json:"dueTime"`
	CompletedTime int64  `json:"completedTime"` // Supernote quirk: holds creation time
	LastModified  int64  `json:"lastModified"`  // Supernote quirk: holds completion time when completed
	Recurrence    string `json:"recurrence,omitempty"`
	IsReminderOn  string `json:"isReminderOn"`
	Links              string `json:"links,omitempty"`
	IsDeleted          string `json:"isDeleted"`
	Sort               int    `json:"sort"`
	SortCompleted      int    `json:"sortCompleted"`
	SortTime           int64  `json:"sortTime,omitempty"`
	PlanerSort         int    `json:"planerSort"`
	PlanerSortTime     int64  `json:"planerSortTime,omitempty"`
	AllSort            int    `json:"allSort"`
	AllSortCompleted   int    `json:"allSortCompleted"`
	AllSortTime        int64  `json:"allSortTime,omitempty"`
}
