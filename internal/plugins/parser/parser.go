package parser

import (
	"context"
	"log/slog"
	"time"

	"github.com/hashicorp/go-hclog"
	cliparser "github.com/infracost/cli/pkg/plugins/parser"
	"github.com/infracost/lsp/internal/plugins/client"
	"github.com/infracost/proto/gen/go/infracost/parser/api"
)

type PluginClient struct {
	Plugin  string
	Version string

	client *client.PluginClient[api.ParserServiceClient]
}

func (c *PluginClient) Load(level hclog.Level) (api.ParserServiceClient, error) {
	if c.client == nil {
		path := c.Plugin
		c.client = client.NewPluginClient(func() (api.ParserServiceClient, func(), error) {
			return cliparser.Connect(path, level)
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

func (c *PluginClient) Reconnect(level hclog.Level) (api.ParserServiceClient, error) {
	if c.client == nil {
		return c.Load(level)
	}
	return c.client.Reconnect()
}

func (c *PluginClient) Close() {
	if c.client != nil {
		c.client.Close()
	}
}
