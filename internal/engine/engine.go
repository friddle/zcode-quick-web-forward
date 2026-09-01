// Package engine implements a client for the ZCode app-server ("ZCode
// Protocol") JSON-RPC stdio protocol. It drives the real engine so phone
// conversation commands (createSession / sendText) execute GLM for real, and
// it relays the engine's streaming events (state.updated, stream.chunk,
// computer-use/operation-event, v4/telemetry/event) back out.
package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Client is a connected app-server (node zcode.cjs app-server) driver.
type Client struct {
	mu      sync.Mutex
	stdin   io.Writer
	nextID  int
	pending map[int]chan json.RawMessage
	// OnEvent is called for every engine->client notification/request line
	// (method present; includes session/requestRuntimePreferences and the
	// v4/telemetry/event / state.updated / computer-use stream).
	OnEvent func(m json.RawMessage)
}

// New returns an empty Client; Attach wires the engine stdin.
func New() *Client {
	return &Client{nextID: 1000, pending: map[int]chan json.RawMessage{}}
}

// Attach binds the engine's stdin.
func (c *Client) Attach(stdin io.Writer) {
	c.mu.Lock()
	c.stdin = stdin
	c.mu.Unlock()
}

// Write sends a raw JSON line to the engine.
func (c *Client) Write(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin == nil {
		return false
	}
	_, err = c.stdin.Write(append(b, '\n'))
	return err == nil
}

// CreateSession asks the engine to create a conversation session and blocks
// until it replies (or times out).
func (c *Client) CreateSession(workspaceKey, workspacePath string, timeout time.Duration) (map[string]any, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	params := map[string]any{
		"workspace": map[string]any{
			"workspaceKey":  workspaceKey,
			"workspacePath": workspacePath,
		},
	}
	if !c.Write(map[string]any{"id": id, "method": "session/create", "params": params}) {
		c.forget(id)
		return nil, fmt.Errorf("engine stdin closed")
	}
	select {
	case line := <-ch:
		c.forget(id)
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &resp) != nil {
			return nil, fmt.Errorf("bad create reply")
		}
		if len(resp.Error) > 0 {
			return nil, fmt.Errorf("session/create: %s", resp.Error)
		}
		var m map[string]any
		if json.Unmarshal(resp.Result, &m) != nil {
			return nil, fmt.Errorf("bad create result")
		}
		return m, nil
	case <-time.After(timeout):
		c.forget(id)
		return nil, fmt.Errorf("session/create timeout")
	}
}

// SendMessage sends a user message to a session (fire-and-forget; streaming
// results arrive via OnEvent).
func (c *Client) SendMessage(sessionID, content string) bool {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	return c.Write(map[string]any{
		"id":     id,
		"method": "session/send",
		"params": map[string]any{
			"sessionId": sessionID,
			"content":   content,
		},
	})
}

// RespondToRequest answers an engine->client request (e.g.
// session/requestRuntimePreferences) with the given result.
func (c *Client) RespondToRequest(reqID any, result any) bool {
	return c.Write(map[string]any{"id": reqID, "result": result})
}

func (c *Client) forget(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// HandleLine processes one engine stdout line.
func (c *Client) HandleLine(line []byte) {
	var head struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(line, &head) != nil {
		return
	}
	if head.Method != "" {
		if c.OnEvent != nil {
			c.OnEvent(json.RawMessage(line))
		}
		return
	}
	var id int
	if json.Unmarshal(head.ID, &id) == nil && id > 0 {
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			ch <- json.RawMessage(line)
		}
	}
}
