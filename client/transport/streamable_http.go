package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

type StreamableHTTPCOption func(*StreamableHTTP)

func WithHTTPHeaders(headers map[string]string) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.headers = headers
	}
}

// WithHTTPTimeout sets the timeout for a HTTP request and stream.
func WithHTTPTimeout(timeout time.Duration) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.httpClient.Timeout = timeout
	}
}

// StreamableHTTP implements Streamable HTTP transport.
//
// It transmits JSON-RPC messages over individual HTTP requests. One message per request.
// The HTTP response body can either be a single JSON-RPC response,
// or an upgraded SSE stream that concludes with a JSON-RPC response for the same request.
//
// https://modelcontextprotocol.io/specification/2025-03-26/basic/transports
type StreamableHTTP struct {
	baseURL    *url.URL
	httpClient *http.Client
	headers    map[string]string

	sessionID atomic.Value // string

	notificationHandler func(mcp.JSONRPCNotification)
	notifyMu            sync.RWMutex

	closed chan struct{}
}

// NewStreamableHTTP creates a new Streamable HTTP transport with the given base URL.
// Returns an error if the URL is invalid.
func NewStreamableHTTP(baseURL string, options ...StreamableHTTPCOption) (*StreamableHTTP, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	smc := &StreamableHTTP{
		baseURL:    parsedURL,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		headers:    make(map[string]string),
		closed:     make(chan struct{}),
	}
	smc.sessionID.Store("") // set initial value to simplify later usage

	for _, opt := range options {
		opt(smc)
	}

	return smc, nil
}

// Start initiates the HTTP connection to the server.
func (c *StreamableHTTP) Start(ctx context.Context) error {
	// For Streamable HTTP, we don't need to establish a persistent connection
	return nil
}

// Close closes the all the HTTP connections to the server.
func (c *StreamableHTTP) Close() error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	// Cancel all in-flight requests
	close(c.closed)

	sessionId := c.sessionID.Load().(string)
	if sessionId != "" {
		c.sessionID.Store("")

		// notify server session closed
		go func() {
			req, err := http.NewRequest(http.MethodDelete, c.baseURL.String(), nil)
			if err != nil {
				fmt.Printf("failed to create close request\n: %v", err)
				return
			}
			req.Header.Set(headerKeySessionID, sessionId)
			res, err := c.httpClient.Do(req)
			if err != nil {
				fmt.Printf("failed to send close request\n: %v", err)
				return
			}
			res.Body.Close()
		}()
	}

	return nil
}

const (
	initializeMethod   = "initialize"
	headerKeySessionID = "Mcp-Session-Id"
)

// sendRequest sends a JSON-RPC request to the server and waits for a response.
// Returns the raw JSON response message or an error if the request fails.
func (c *StreamableHTTP) SendRequest(
	ctx context.Context,
	request JSONRPCRequest,
) (*JSONRPCResponse, error) {

	// Create a combined context that could be canceled when the client is closed
	var cancelRequest context.CancelFunc
	ctx, cancelRequest = context.WithCancel(ctx)
	defer cancelRequest()
	go func() {
		select {
		case <-c.closed:
			cancelRequest()
		case <-ctx.Done():
			// The original context was canceled, no need to do anything
		}
	}()

	// Marshal request
	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL.String(), bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	sessionID := c.sessionID.Load()
	if sessionID != "" {
		req.Header.Set(headerKeySessionID, sessionID.(string))
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Check if we got an error response
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		// handle session closed
		if resp.StatusCode == http.StatusNotFound {
			c.sessionID.CompareAndSwap(sessionID, "")
			return nil, fmt.Errorf("session terminated (404)")
		}

		// handle error response
		var errResponse JSONRPCResponse
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &errResponse); err == nil {
			return &errResponse, nil
		}
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, body)
	}

	if request.Method == initializeMethod {
		// saved the received session ID in the response
		// empty session ID is allowed
		if sessionID := resp.Header.Get(headerKeySessionID); sessionID != "" {
			c.sessionID.Store(sessionID)
		}
	}

	// Handle different response types
	switch resp.Header.Get("Content-Type") {
	case "application/json":
		// Single response
		var response JSONRPCResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}

		// should not be a notification
		if response.ID == nil {
			return nil, fmt.Errorf("response should contain RPC id: %v", response)
		}

		return &response, nil

	case "text/event-stream":
		// Server is using SSE for streaming responses
		return c.handleSSEResponse(ctx, resp.Body)

	default:
		return nil, fmt.Errorf("unexpected content type: %s", resp.Header.Get("Content-Type"))
	}
}

// handleSSEResponse processes an SSE stream for a specific request.
// It returns the final result for the request once received, or an error.
func (c *StreamableHTTP) handleSSEResponse(ctx context.Context, reader io.ReadCloser) (*JSONRPCResponse, error) {

	// Create a channel for this specific request
	responseChan := make(chan *JSONRPCResponse, 1)
	defer close(responseChan)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Start a goroutine to process the SSE stream
	go c.readSSE(ctx, reader, func(event, data string) {

		// unsupported
		// - batching
		// - server -> client request

		var message JSONRPCResponse
		if err := json.Unmarshal([]byte(data), &message); err != nil {
			fmt.Printf("failed to unmarshal message: %v", err)
			return
		}

		// Handle notification
		if message.ID == nil {
			var notification mcp.JSONRPCNotification
			if err := json.Unmarshal([]byte(data), &notification); err != nil {
				fmt.Printf("failed to unmarshal notification: %v", err)
				return
			}
			c.notifyMu.RLock()
			if c.notificationHandler != nil {
				c.notificationHandler(notification)
			}
			c.notifyMu.RUnlock()
			return
		}

		responseChan <- &message
	})

	// Wait for the response or context cancellation
	select {
	case response := <-responseChan:
		if response == nil {
			return nil, fmt.Errorf("unexpected nil response")
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readSSE reads the SSE stream(reader) and calls the handler for each event and data pair.
// It will end when the reader is closed (or the context is done).
func (c *StreamableHTTP) readSSE(ctx context.Context, reader io.ReadCloser, handler func(event, data string)) {
	defer reader.Close()

	br := bufio.NewReader(reader)
	var event, data string

	for {
		select {
		case <-ctx.Done():
			return
		default:
			line, err := br.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					// Process any pending event before exit
					if event != "" && data != "" {
						handler(event, data)
					}
					return
				}
				select {
				case <-ctx.Done():
					return
				default:
					fmt.Printf("SSE stream error: %v\n", err)
					return
				}
			}

			// Remove only newline markers
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				// Empty line means end of event
				if event != "" && data != "" {
					handler(event, data)
					event = ""
					data = ""
				}
				continue
			}

			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}
}

func (c *StreamableHTTP) SendNotification(ctx context.Context, notification mcp.JSONRPCNotification) error {

	// Marshal request
	requestBody, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL.String(), bytes.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if sessionID := c.sessionID.Load(); sessionID != "" {
		req.Header.Set(headerKeySessionID, sessionID.(string))
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"notification failed with status %d: %s",
			resp.StatusCode,
			body,
		)
	}

	return nil
}

func (c *StreamableHTTP) SetNotificationHandler(handler func(mcp.JSONRPCNotification)) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.notificationHandler = handler
}
