package providers

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/infracost/cli/pkg/plugins/pluginconn"
	cliprovider "github.com/infracost/cli/pkg/plugins/providers"
	"github.com/infracost/lsp/internal/plugins/client"
	"github.com/infracost/lsp/internal/plugins/pluginlog"
	proto "github.com/infracost/proto/gen/go/infracost/provider"
)

type PluginClient struct {
	AWS    string
	Google string
	Azure  string

	AWSVersion    string
	AzureVersion  string
	GoogleVersion string

	mu      sync.Mutex
	clients map[proto.Provider]*client.PluginClient[proto.ProviderServiceClient]
}

func (c *PluginClient) Load(provider proto.Provider) (proto.ProviderServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.clients == nil {
		c.clients = make(map[proto.Provider]*client.PluginClient[proto.ProviderServiceClient])
	}

	if pc, ok := c.clients[provider]; ok {
		return pc.Client()
	}

	path, err := c.path(provider)
	if err != nil {
		return nil, err
	}

	kind := "provider:" + provider.String()
	pc := client.NewPluginClient(func() (proto.ProviderServiceClient, func(), error) {
		cl, cleanup, err := cliprovider.ConnectWithOptions(path, pluginconn.ConnectOptions{
			Logger: pluginlog.New(slog.Default().With("plugin", "provider", "provider", provider.String())),
		})
		if err != nil {
			pluginlog.LogConnectError(kind, path, err)
			return nil, nil, err
		}
		return cl, cleanup, nil
	})
	c.clients[provider] = pc
	return pc.Client()
}

func (c *PluginClient) Reconnect(provider proto.Provider) (proto.ProviderServiceClient, error) {
	c.mu.Lock()
	if pc, ok := c.clients[provider]; ok {
		c.mu.Unlock()
		return pc.Reconnect()
	}
	c.mu.Unlock()
	return c.Load(provider)
}

func (c *PluginClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pc := range c.clients {
		pc.Close()
	}
}

func (c *PluginClient) path(provider proto.Provider) (string, error) {
	switch provider {
	case proto.Provider_PROVIDER_AWS:
		return c.AWS, nil
	case proto.Provider_PROVIDER_GOOGLE:
		return c.Google, nil
	case proto.Provider_PROVIDER_AZURERM:
		return c.Azure, nil
	default:
		return "", fmt.Errorf("unknown provider: %s", provider)
	}
}
