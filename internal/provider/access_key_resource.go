package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/zvdy/terraform-provider-lsmrust/internal/client"
)

var (
	_ resource.Resource                = &accessKeyResource{}
	_ resource.ResourceWithConfigure   = &accessKeyResource{}
	_ resource.ResourceWithImportState = &accessKeyResource{}
)

func NewAccessKeyResource() resource.Resource {
	return &accessKeyResource{}
}

type accessKeyResource struct {
	client *client.Client
}

type accessKeyModel struct {
	ID         types.String `tfsdk:"id"`
	DatabaseID types.String `tfsdk:"database_id"`
	Scopes     types.Set    `tfsdk:"scopes"`
	Secret     types.String `tfsdk:"secret"`
}

func (r *accessKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_access_key"
}

func (r *accessKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An access key for a database. The secret is returned by the control plane only once, at creation, " +
			"and is kept in Terraform state from then on.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Control-plane identifier of the access key.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"database_id": schema.StringAttribute{
				MarkdownDescription: "ID of the database this key grants access to. Changing it creates a new key.",
				Required:            true,
				Validators:          []validator.String{notBlank()},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"scopes": schema.SetAttribute{
				MarkdownDescription: "Permissions granted to the key: any of `read`, `write`, `admin`. Order does not matter. Can be changed in place.",
				ElementType:         types.StringType,
				Required:            true,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.OneOf("read", "write", "admin")),
				},
			},
			"secret": schema.StringAttribute{
				MarkdownDescription: "The key's secret. Only available when the key is created; it is null for imported keys.",
				Computed:            true,
				Sensitive:           true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *accessKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, errMsg := clientFromProviderData(req.ProviderData)
	if errMsg != "" {
		resp.Diagnostics.AddError("Unexpected provider data", errMsg)
		return
	}
	r.client = c
}

func (r *accessKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan accessKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scopes, diags := scopesFromSet(ctx, plan.Scopes)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := r.client.CreateAccessKey(ctx, plan.DatabaseID.ValueString(), scopes)
	if err != nil {
		resp.Diagnostics.AddError("Error creating access key", err.Error())
		return
	}
	if key.ID == "" {
		resp.Diagnostics.AddError("Error creating access key", "The control plane response did not include an access key id.")
		return
	}

	secret := types.StringValue(key.Secret)
	if key.Secret == "" {
		secret = types.StringNull()
		resp.Diagnostics.AddWarning("Access key created without a secret",
			fmt.Sprintf("The control plane did not return a secret for access key %s. It cannot be retrieved later.", key.ID))
	}
	state, diags := accessKeyFromAPI(ctx, key, secret)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *accessKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state accessKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := r.client.GetAccessKey(ctx, state.ID.ValueString())
	if errors.Is(err, client.ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error reading access key", err.Error())
		return
	}

	// The API never returns the secret after creation; keep what we have.
	newState, diags := accessKeyFromAPI(ctx, key, state.Secret)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, newState)...)
}

func (r *accessKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state accessKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scopes, diags := scopesFromSet(ctx, plan.Scopes)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()
	if err := r.client.UpdateAccessKey(ctx, id, scopes); err != nil {
		resp.Diagnostics.AddError("Error updating access key", err.Error())
		r.restoreState(ctx, id, state, resp)
		return
	}
	key, err := r.client.GetAccessKey(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Error reading access key after update", err.Error())
		resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
		return
	}

	newState, diags := accessKeyFromAPI(ctx, key, state.Secret)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, newState)...)
}

func (r *accessKeyResource) restoreState(ctx context.Context, id string, prior accessKeyModel, resp *resource.UpdateResponse) {
	if key, err := r.client.GetAccessKey(ctx, id); err == nil {
		restored, diags := accessKeyFromAPI(ctx, key, prior.Secret)
		if !diags.HasError() {
			resp.Diagnostics.Append(resp.State.Set(ctx, restored)...)
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, prior)...)
}

func (r *accessKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state accessKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.DeleteAccessKey(ctx, state.ID.ValueString())
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		resp.Diagnostics.AddError("Error deleting access key", err.Error())
	}
}

func (r *accessKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func accessKeyFromAPI(ctx context.Context, key *client.AccessKey, secret types.String) (accessKeyModel, diag.Diagnostics) {
	scopes, diags := types.SetValueFrom(ctx, types.StringType, key.Scopes)
	return accessKeyModel{
		ID:         types.StringValue(key.ID),
		DatabaseID: types.StringValue(key.DatabaseID),
		Scopes:     scopes,
		Secret:     secret,
	}, diags
}

func scopesFromSet(ctx context.Context, set types.Set) ([]string, diag.Diagnostics) {
	var scopes []string
	diags := set.ElementsAs(ctx, &scopes, false)
	slices.Sort(scopes)
	return scopes, diags
}
