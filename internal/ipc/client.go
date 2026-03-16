// client.go – IPC HTTP client for the EchoSend CLI
//
// Every CLI sub-command (--send, --history, --add, etc.) that needs to talk
// to a running daemon calls helpers in this file rather than speaking HTTP
// directly.  This keeps all the wire-level detail in one place and makes the
// cmd/ layer trivially thin.
//
// If the daemon is not running the helpers return ErrDaemonNotRunning so the
// caller can print a helpful message and exit cleanly.
package ipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"p2p-sync/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// Sentinel errors
// ─────────────────────────────────────────────────────────────────────────────

// ErrDaemonNotRunning is returned when the IPC server is not reachable.
// Callers should print a user-friendly hint such as "run 'echosend daemon' first".
var ErrDaemonNotRunning = errors.New("EchoSend daemon is not running (start it with: echosend daemon)")

// ─────────────────────────────────────────────────────────────────────────────
// Client
// ─────────────────────────────────────────────────────────────────────────────

// Client is a thin HTTP client scoped to a single IPC port.
// All methods are safe for concurrent use from multiple goroutines.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient constructs a Client that targets the IPC server listening on
// 127.0.0.1:ipcPort.
func NewClient(ipcPort int) *Client {
	return &Client{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", ipcPort),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			// Never follow redirects from localhost; if we get one something
			// is badly wrong.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Liveness
// ─────────────────────────────────────────────────────────────────────────────

// Ping sends a GET /api/ping to the daemon and returns nil if it responds.
// Returns ErrDaemonNotRunning if the connection is refused or times out.
func (c *Client) Ping() error {
	resp, err := c.get("/api/ping")
	if err != nil {
		return ErrDaemonNotRunning
	}
	defer resp.Body.Close()

	var reply models.IPCReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return fmt.Errorf("ipc ping: decode response: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("ipc ping: daemon returned error: %s", reply.Message)
	}
	return nil
}

// IsDaemonRunning is a convenience wrapper around Ping that returns a bool
// instead of an error.  Use it for branching logic in the CLI.
func (c *Client) IsDaemonRunning() bool {
	return c.Ping() == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Send message
// ─────────────────────────────────────────────────────────────────────────────

// SendMessage broadcasts content as a text message through the daemon.
// Returns the persisted Message on success.
func (c *Client) SendMessage(content string) (*models.Message, error) {
	body := models.IPCSendMessageReq{Content: content}

	resp, err := c.postJSON("/api/send/message", body)
	if err != nil {
		return nil, wrapConnErr(err)
	}
	defer resp.Body.Close()

	var reply models.IPCReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("send message: decode response: %w", err)
	}
	if !reply.OK {
		return nil, fmt.Errorf("send message: %s", reply.Message)
	}

	// The daemon echoes back the persisted Message in Data.
	// We do a round-trip through JSON to avoid unsafe type assertions.
	var msg models.Message
	if reply.Data != nil {
		dataBytes, err := json.Marshal(reply.Data)
		if err != nil {
			return nil, fmt.Errorf("send message: re-marshal data: %w", err)
		}
		if err := json.Unmarshal(dataBytes, &msg); err != nil {
			return nil, fmt.Errorf("send message: unmarshal message: %w", err)
		}
	}
	return &msg, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Send file
// ─────────────────────────────────────────────────────────────────────────────

// SendFile asks the daemon to hash and broadcast the file at localPath.
// The operation is asynchronous on the daemon side: this call returns as soon
// as the daemon acknowledges the request (HTTP 202 Accepted).
func (c *Client) SendFile(localPath string) error {
	body := models.IPCSendFileReq{Path: localPath}

	resp, err := c.postJSON("/api/send/file", body)
	if err != nil {
		return wrapConnErr(err)
	}
	defer resp.Body.Close()

	var reply models.IPCReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return fmt.Errorf("send file: decode response: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("send file: %s", reply.Message)
	}
	return nil
}

// PullFileByHash asks the daemon to manually pull/retry a file by SHA-256 hash.
// The daemon schedules the pull asynchronously and returns immediately.
func (c *Client) PullFileByHash(fileHash string) error {
	body := models.IPCPullFileReq{FileHash: fileHash}

	resp, err := c.postJSON("/api/pull/file", body)
	if err != nil {
		return wrapConnErr(err)
	}
	defer resp.Body.Close()

	var reply models.IPCReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return fmt.Errorf("pull file: decode response: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("pull file: %s", reply.Message)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Add peer
// ─────────────────────────────────────────────────────────────────────────────

// AddPeer asks the daemon to add target (IP or CIDR) to its StaticPeers list.
func (c *Client) AddPeer(target string) error {
	body := models.IPCAddPeerReq{Target: target}

	resp, err := c.postJSON("/api/peers/add", body)
	if err != nil {
		return wrapConnErr(err)
	}
	defer resp.Body.Close()

	var reply models.IPCReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return fmt.Errorf("add peer: decode response: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("add peer: %s", reply.Message)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// History
// ─────────────────────────────────────────────────────────────────────────────

// History fetches up to limit message and file records from the daemon,
// ordered newest-first.  Pass limit <= 0 to use the server default (50).
func (c *Client) History(limit int) (*models.IPCHistoryResp, error) {
	url := "/api/history"
	if limit > 0 {
		url = fmt.Sprintf("%s?limit=%d", url, limit)
	}

	resp, err := c.get(url)
	if err != nil {
		return nil, wrapConnErr(err)
	}
	defer resp.Body.Close()

	var reply models.IPCReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("history: decode response: %w", err)
	}
	if !reply.OK {
		return nil, fmt.Errorf("history: %s", reply.Message)
	}

	// Unmarshal the nested Data field into IPCHistoryResp.
	dataBytes, err := json.Marshal(reply.Data)
	if err != nil {
		return nil, fmt.Errorf("history: re-marshal data: %w", err)
	}
	var histResp models.IPCHistoryResp
	if err := json.Unmarshal(dataBytes, &histResp); err != nil {
		return nil, fmt.Errorf("history: unmarshal history: %w", err)
	}
	return &histResp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Status
// ─────────────────────────────────────────────────────────────────────────────

// Status fetches the daemon's node info and known-peer list.
func (c *Client) Status() (*models.IPCStatusResp, error) {
	resp, err := c.get("/api/status")
	if err != nil {
		return nil, wrapConnErr(err)
	}
	defer resp.Body.Close()

	var reply models.IPCReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("status: decode response: %w", err)
	}
	if !reply.OK {
		return nil, fmt.Errorf("status: %s", reply.Message)
	}

	dataBytes, err := json.Marshal(reply.Data)
	if err != nil {
		return nil, fmt.Errorf("status: re-marshal data: %w", err)
	}
	var statusResp models.IPCStatusResp
	if err := json.Unmarshal(dataBytes, &statusResp); err != nil {
		return nil, fmt.Errorf("status: unmarshal status: %w", err)
	}
	return &statusResp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Low-level HTTP helpers
// ─────────────────────────────────────────────────────────────────────────────

// get performs a GET request to path (relative to the IPC base URL) and
// returns the raw *http.Response.  The caller is responsible for closing the
// response body.
func (c *Client) get(path string) (*http.Response, error) {
	url := c.baseURL + path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	return c.httpClient.Do(req)
}

// postJSON marshals body as JSON and POSTs it to path.
// Returns the raw *http.Response; the caller must close the body.
func (c *Client) postJSON(path string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")

	return c.httpClient.Do(req)
}

// readAll is a convenience wrapper that reads the full body of an
// *http.Response into a byte slice (used in debug/logging helpers).
func readAll(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	return b
}

// wrapConnErr converts a low-level connection error (e.g. "connection refused")
// into the user-friendly ErrDaemonNotRunning sentinel so callers don't have to
// inspect raw error strings.
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}

	// Only map explicit connect failures to ErrDaemonNotRunning.
	// Do not classify EOF/timeouts as daemon-not-running, because those can
	// happen after the daemon already accepted and processed the request.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var opErr *net.OpError
		if errors.As(urlErr.Err, &opErr) && opErr.Op == "dial" {
			return ErrDaemonNotRunning
		}

		msg := strings.ToLower(urlErr.Error())
		for _, needle := range []string{
			"connection refused",
			"actively refused",
			"no connection could be made",
			"no such host",
			"cannot assign requested address",
		} {
			if strings.Contains(msg, needle) {
				return ErrDaemonNotRunning
			}
		}
	}

	return err
}
