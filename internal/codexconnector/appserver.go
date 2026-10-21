package codexconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// appServer is a JSON-RPC client for Codex's app-server over its control
// socket (WebSocket frames, no "jsonrpc" field). Responses resolve pending
// calls; notifications go to notes. Requests from the server (approvals)
// are declined: nobody is here to answer them.
type appServer struct {
	c      *websocket.Conn
	nextID atomic.Int64
	mu     sync.Mutex
	wait   map[int64]chan rpcReply
	notes  chan note
	done   chan struct{}
	err    error
}

type note struct {
	Method string
	Params json.RawMessage
}

type rpcReply struct {
	Result json.RawMessage
	Err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("app-server: %s (%d)", e.Message, e.Code) }

func dialAppServer(ctx context.Context, socket string) (*appServer, error) {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	c, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(64 << 20)
	a := &appServer{c: c, wait: map[int64]chan rpcReply{}, notes: make(chan note, 256), done: make(chan struct{})}
	go a.read()
	if _, err := a.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{
		"name": "wakeshift", "title": "Wakeshift connector", "version": "0.0.1"}}); err != nil {
		c.CloseNow()
		return nil, err
	}
	if err := a.send(ctx, map[string]any{"method": "initialized"}); err != nil {
		c.CloseNow()
		return nil, err
	}
	return a, nil
}

func (a *appServer) read() {
	defer close(a.done)
	for {
		_, b, err := a.c.Read(context.Background())
		if err != nil {
			a.err = err
			return
		}
		var m struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params json.RawMessage  `json:"params"`
			Result json.RawMessage  `json:"result"`
			Error  *rpcError        `json:"error"`
		}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch {
		case m.ID != nil && m.Method != "":
			a.send(context.Background(), map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "the Wakeshift connector can't answer interactive requests"}})
		case m.ID != nil:
			var id int64
			json.Unmarshal(*m.ID, &id)
			a.mu.Lock()
			ch := a.wait[id]
			delete(a.wait, id)
			a.mu.Unlock()
			if ch != nil {
				ch <- rpcReply{Result: m.Result, Err: m.Error}
			}
		case m.Method != "":
			select {
			case a.notes <- note{m.Method, m.Params}:
			default: // nobody's reading fast enough; status is re-read before each turn
			}
		}
	}
}

func (a *appServer) send(ctx context.Context, m map[string]any) error {
	b, _ := json.Marshal(m)
	return a.c.Write(ctx, websocket.MessageText, b)
}

func (a *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := a.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	a.mu.Lock()
	a.wait[id] = ch
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.wait, id); a.mu.Unlock() }()
	if err := a.send(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Result, nil
	case <-a.done:
		return nil, errors.New("app-server disconnected")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *appServer) close() { a.c.Close(websocket.StatusNormalClosure, "") }
