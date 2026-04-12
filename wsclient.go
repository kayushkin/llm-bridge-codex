package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// NotificationHandler handles a server notification (no response expected).
type NotificationHandler func(method string, params json.RawMessage)

// RequestHandler handles a server->client request and returns a result or error.
type RequestHandler func(method string, params json.RawMessage) (json.RawMessage, error)

// RPCRequest is a JSON-RPC 2.0 request or notification.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// rawMessage is the union of all JSON-RPC message shapes for dispatch.
type rawMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m *rawMessage) isResponse() bool {
	return m.Method == "" && (m.Result != nil || m.Error != nil)
}

func (m *rawMessage) isServerRequest() bool {
	return m.Method != "" && m.ID != nil && string(m.ID) != "null"
}

func (m *rawMessage) isNotification() bool {
	return m.Method != "" && (m.ID == nil || string(m.ID) == "null")
}

func (m *rawMessage) parseID() int64 {
	if m.ID == nil {
		return 0
	}
	var id int64
	_ = json.Unmarshal(m.ID, &id)
	return id
}

// WSClient is a JSON-RPC 2.0 multiplexer over a WebSocket connection.
type WSClient struct {
	conn *websocket.Conn
	mu   sync.Mutex // protects conn writes

	nextID  atomic.Int64
	pending sync.Map // map[int64]chan *RPCResponse

	notifMu       sync.RWMutex
	notifHandlers map[string]NotificationHandler

	reqMu       sync.RWMutex
	reqHandlers map[string]RequestHandler

	done chan struct{}
}

func NewWSClient(conn *websocket.Conn) *WSClient {
	return &WSClient{
		conn:          conn,
		notifHandlers: make(map[string]NotificationHandler),
		reqHandlers:   make(map[string]RequestHandler),
		done:          make(chan struct{}),
	}
}

func (c *WSClient) OnNotification(method string, h NotificationHandler) {
	c.notifMu.Lock()
	defer c.notifMu.Unlock()
	c.notifHandlers[method] = h
}

func (c *WSClient) OnRequest(method string, h RequestHandler) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	c.reqHandlers[method] = h
}

func (c *WSClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
	}

	ch := make(chan *RPCResponse, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	req := RPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}

	if err := c.writeJSON(req); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("connection closed")
	}
}

func (c *WSClient) Notify(method string, params any) error {
	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
	}

	req := RPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}
	return c.writeJSON(req)
}

func (c *WSClient) Done() <-chan struct{} {
	return c.done
}

func (c *WSClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}

func (c *WSClient) ReadPump() {
	defer close(c.done)

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[ws] read error: %v", err)
			}
			return
		}

		var msg rawMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[ws] unmarshal error: %v", err)
			continue
		}

		switch {
		case msg.isResponse():
			c.handleResponse(&msg)
		case msg.isServerRequest():
			c.handleServerRequest(&msg)
		case msg.isNotification():
			c.handleNotification(&msg)
		}
	}
}

func (c *WSClient) handleResponse(msg *rawMessage) {
	id := msg.parseID()
	if ch, ok := c.pending.LoadAndDelete(id); ok {
		ch.(chan *RPCResponse) <- &RPCResponse{
			JSONRPC: msg.JSONRPC,
			ID:      id,
			Result:  msg.Result,
			Error:   msg.Error,
		}
	}
}

func (c *WSClient) handleServerRequest(msg *rawMessage) {
	id := msg.parseID()

	c.reqMu.RLock()
	handler, ok := c.reqHandlers[msg.Method]
	c.reqMu.RUnlock()

	if !ok {
		c.sendResponse(id, nil, &RPCError{Code: -32601, Message: "method not found"})
		return
	}

	go func() {
		result, err := handler(msg.Method, msg.Params)
		if err != nil {
			c.sendResponse(id, nil, &RPCError{Code: -32000, Message: err.Error()})
		} else {
			c.sendResponse(id, result, nil)
		}
	}()
}

func (c *WSClient) handleNotification(msg *rawMessage) {
	c.notifMu.RLock()
	handler, ok := c.notifHandlers[msg.Method]
	c.notifMu.RUnlock()

	if ok {
		handler(msg.Method, msg.Params)
	}
}

func (c *WSClient) sendResponse(id int64, result json.RawMessage, rpcErr *RPCError) {
	resp := RPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
		Error:   rpcErr,
	}
	if err := c.writeJSON(resp); err != nil {
		log.Printf("[ws] failed to send response id=%d: %v", id, err)
	}
}

func (c *WSClient) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(v)
}
