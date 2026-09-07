package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// MsgPackMaxBody mirrors the server's default max_payload_size (1 GiB).
// The server buffers the whole document, so there is no streaming
// benefit beyond not holding it twice on the client.
const MsgPackMaxBody = 1 << 30

// WriteMsgPack posts a complete MessagePack document to
// POST /api/v1/write/msgpack (write tier). The body is sent as-is: the
// server accepts its columnar `{m, columns}`, row `{m,t,fields,tags}`,
// and `{batch:[…]}` shapes, and sniffs gzip/zstd by magic bytes, so a
// compressed file can be passed straight through. size is the exact
// body length when known (a file), or -1 to send chunked (stdin).
//
// Success is 204 with an empty body. Note the server silently drops
// batch items or array elements that fail to decode and still answers
// 204; only a whole-document failure is a 400.
func (c *Client) WriteMsgPack(ctx context.Context, body io.Reader, size int64, database string) error {
	if size > MsgPackMaxBody {
		return fmt.Errorf("body is %d bytes; the server accepts at most %d (1 GiB)", size, MsgPackMaxBody)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+"/api/v1/write/msgpack", body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/msgpack")
	req.Header.Set("Accept", "application/json")
	c.setCommonHeaders(req, database)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("write msgpack: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return decodeWriteError(resp.StatusCode, respBody)
}
