package control

import (
	"context"
	"net"

	"github.com/lxdb/bsbctl/sdk/rpc"
)

type Client struct {
	peer   *rpc.Peer
	cancel context.CancelFunc
}

func Dial(ctx context.Context, path string) (*Client, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	peer := rpc.NewPeer(conn)
	go func() { _ = peer.Serve(serveCtx) }()
	return &Client{peer: peer, cancel: cancel}, nil
}

func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return c.peer.Call(ctx, method, params, result)
}

func (c *Client) Close() error {
	c.cancel()
	return c.peer.Close()
}
