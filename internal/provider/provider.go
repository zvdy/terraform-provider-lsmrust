package provider

import (
	"context"
	"os"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/zvdy/terraform-provider-lsmrust/internal/client"
)

const (
	envEndpoint = "LSMRUST_ENDPOINT"
	envAPIKey   = "LSMRUST_API_KEY"
)

var _ provider.Provider = &lsmrustProvider{}

type lsmrustProvider struct {
	version string
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIKey   types.String `tfsdk:"api_key"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &lsmrustProvider{version: version}
	}
}

func (p *lsmrustProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "lsmrust"
	resp.Version = p.version
}

func (p *lsmrustProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages tenant databases on the lsm-rust hosted control plane.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "Base URL of the lsm-rust control-plane API, e.g. `http://localhost:8080`. " +
					"If unset or empty, the `" + envEndpoint + "` environment variable is used.",
				Optional: true,
			},
			"api_key": schema.StringAttribute{
				MarkdownDescription: "API key sent as a bearer token. If unset or empty, the `" + envAPIKey + "` environment variable is used.",
				Optional:            true,
				Sensitive:           true,
			},
		},
	}
}

func (p *lsmrustProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Unknown lsmrust endpoint",
			"The provider cannot be configured with an endpoint that is unknown until apply. Use a static value or the "+envEndpoint+" environment variable.")
	}
	if cfg.APIKey.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Unknown lsmrust API key",
			"The provider cannot be configured with an API key that is unknown until apply. Use a static value or the "+envAPIKey+" environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := cfg.Endpoint.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv(envEndpoint)
	}
	apiKey := cfg.APIKey.ValueString()
	if apiKey == "" {
		apiKey = os.Getenv(envAPIKey)
	}

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Missing lsmrust endpoint",
			"Set the endpoint argument in the provider block or the "+envEndpoint+" environment variable.")
		return
	}

	c, err := client.New(endpoint, apiKey, nil)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid lsmrust endpoint", err.Error())
		return
	}
	resp.ResourceData = c
}

func (p *lsmrustProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewDatabaseResource,
		NewAccessKeyResource,
	}
}

func (p *lsmrustProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

func notBlank() validator.String {
	return stringvalidator.RegexMatches(regexp.MustCompile(`\S`), "must not be empty or only whitespace")
}

func clientFromProviderData(data any) (*client.Client, string) {
	if data == nil {
		return nil, ""
	}
	c, ok := data.(*client.Client)
	if !ok {
		return nil, "expected *client.Client provider data; this is a bug in the provider"
	}
	return c, ""
}
