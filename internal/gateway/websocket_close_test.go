package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWebSocketRelayPreservesUpstreamCloseStatus(t *testing.T) {
	for _, status := range []websocket.StatusCode{websocket.StatusNormalClosure, websocket.StatusMessageTooBig, websocket.StatusTryAgainLater, 4001} {
		t.Run(status.String(), func(t *testing.T) {
			g, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, _, err := conn.Read(ctx); err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close(status, "upstream message limit")
			}, nil, "")
			c := openWS(t, g)
			sendWS(t, c, wsObject{"type": "response.create", "model": "chatgpt/test", "input": []any{}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, err := c.Read(ctx)
			if websocket.CloseStatus(err) != status {
				t.Fatalf("upstream close status %d was lost: %v", status, err)
			}
		})
	}
}
