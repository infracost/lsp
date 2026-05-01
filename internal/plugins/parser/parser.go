package parser

import (
	"context"
	"log/slog"
	"time"

	cliparser "github.com/infracost/cli/pkg/plugins/parser"
	"github.com/infracost/cli/pkg/plugins/pluginconn"
	"github.com/infracost/lsp/internal/plugins/client"
	"github.com/infracost/lsp/internal/plugins/pluginlog"
	"github.com/infracost/proto/gen/go/infracost/parser/api"
)

type PluginClient struct {
	Plugin  string
	Version string

	client *client.PluginClient[api.ParserServiceClient]
}

func (c *PluginClient) Load() (api.ParserServiceClient, error) {
	if c.client == nil {
		path := c.Plugin
		c.client = client.NewPluginClient(func() (api.ParserServiceClient, func(), error) {
			cl, cleanup, err := cliparser.ConnectWithOptions(path, pluginconn.ConnectOptions{
				Logger: pluginlog.New(slog.Default().With("plugin", "parser")),
			})
			if err != nil {
				pluginlog.LogConnectError("parser", path, err)
				return nil, nil, err
			}
			return cl, cleanup, nil
		})
		c.client.OnConnect(func(cl api.ParserServiceClient) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			slog.Debug("parser: initializing plugin")
			_, err := cl.Initialize(ctx, &api.InitializeRequest{DisableGraphCache: true})
			return err
		})
	}
	return c.client.Client()
}

func (c *PluginClient) Reconnect() (api.ParserServiceClient, error) {
	if c.client == nil {
		return c.Load()
	}
	return c.client.Reconnect()
}

func (c *PluginClient) Close() {
	if c.client != nil {
		c.client.Close()
	}
}
