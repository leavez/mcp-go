package client

import (
	"context"
	"fmt"
	"io"

	"github.com/mark3labs/mcp-go/client/transport"
)

// NewStdioMCPClient creates a new stdio-based MCP client that communicates with a subprocess.
// It launches the specified command with given arguments and sets up stdin/stdout pipes for communication.
// Returns an error if the subprocess cannot be started or the pipes cannot be created.
//
// NewStdioMCPClient will start the connection automatically. Don't call the Start method manually.
func NewStdioMCPClient(
	command string,
	env []string,
	args ...string,
) (*Client, error) {

	stdioTransport := transport.NewStdio(command, env, args...)
	err := stdioTransport.Start(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to start stdio transport: %w", err)
	}

	return NewClient(stdioTransport), nil
}

// GetStderr returns a reader for the stderr output of the subprocess.
// This can be used to capture error messages or logs from the subprocess.
//
// Note: This method only works with stdio transport.
func GetStderr(c *Client) io.Reader {
	t := c.GetTransport()
	stdio := t.(*transport.Stdio)
	return stdio.Stderr()
}
