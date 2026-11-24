package scanner

import (
	"context"
	"testing"

	cliplugins "github.com/infracost/cli/pkg/plugins"
	repoconfig "github.com/infracost/config"
	pluginpb "github.com/infracost/proto/gen/go/infracost/plugin"
	"github.com/infracost/proto/gen/go/infracost/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// An appcode project is only parsed with the run parameters' enableAiPlugin.
func TestScanProjectSkipsAppCodeUnlessAIEnabled(t *testing.T) {
	project := &repoconfig.Project{Name: "app", Path: ".", Type: "appcode"}

	for name, enabled := range map[string]bool{"AI off": false, "AI on": true} {
		t.Run(name, func(t *testing.T) {
			spy := &deadlineSpyParser{}
			s := scannerWithSpyParser(t, spy)
			s.Plugins.LoadProviderPlugins = func(context.Context) ([]*cliplugins.ProviderPlugin, error) { return nil, nil }

			result, err := s.scanProject(context.Background(), t.TempDir(), &repoconfig.Config{}, project, "", nil, "main", runParamsSnapshot{aiEnabled: enabled})
			if !enabled {
				require.NoError(t, err)
				assert.Equal(t, &ScanResult{}, result)
			}
			assert.Equal(t, enabled, spy.called)
		})
	}
}

type countingProvider struct {
	pluginpb.ProviderServiceClient
	runs *int
}

func (c countingProvider) Process(context.Context, *pluginpb.ProcessRequest, ...grpc.CallOption) (*pluginpb.ProcessResponse, error) {
	*c.runs++
	return &pluginpb.ProcessResponse{}, nil
}

// The ai provider only runs with the run parameters' enableAiPlugin.
func TestProcessLoadedProvidersSkipsAIUnlessEnabled(t *testing.T) {
	for name, enabled := range map[string]bool{"AI off": false, "AI on": true} {
		t.Run(name, func(t *testing.T) {
			awsRuns, aiRuns := 0, 0
			s := &Scanner{Plugins: &cliplugins.Config{
				LoadProviderPlugins: func(context.Context) ([]*cliplugins.ProviderPlugin, error) {
					return []*cliplugins.ProviderPlugin{
						{ProviderServiceClient: countingProvider{runs: &awsRuns}, Info: &pluginpb.GetPluginInfoResponse{Name: "infracost/aws"}},
						{ProviderServiceClient: countingProvider{runs: &aiRuns}, Info: &pluginpb.GetPluginInfoResponse{Name: "infracost/ai"}},
					}, nil
				},
			}}

			s.processLoadedProviders(context.Background(), &provider.TreeInput{}, enabled)
			assert.Equal(t, 1, awsRuns)
			assert.Equal(t, map[bool]int{false: 0, true: 1}[enabled], aiRuns)
		})
	}
}
