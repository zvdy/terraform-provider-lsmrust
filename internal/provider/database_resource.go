package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/objectvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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
	databaseReadyTimeout = 10 * time.Minute
	databasePollInterval = 500 * time.Millisecond
)

var (
	_ resource.Resource                = &databaseResource{}
	_ resource.ResourceWithConfigure   = &databaseResource{}
	_ resource.ResourceWithImportState = &databaseResource{}
)

func NewDatabaseResource() resource.Resource {
	return &databaseResource{}
}

type databaseResource struct {
	client *client.Client
}

type databaseModel struct {
	ID             types.String         `tfsdk:"id"`
	Name           types.String         `tfsdk:"name"`
	Tier           types.String         `tfsdk:"tier"`
	Status         types.String         `tfsdk:"status"`
	ResourceLimits *resourceLimitsModel `tfsdk:"resource_limits"`
	BackupPolicy   *backupPolicyModel   `tfsdk:"backup_policy"`
}

type resourceLimitsModel struct {
	MemoryMB types.Int64 `tfsdk:"memory_mb"`
	DiskGB   types.Int64 `tfsdk:"disk_gb"`
}

type backupPolicyModel struct {
	Schedule  types.String `tfsdk:"schedule"`
	Retention types.String `tfsdk:"retention"`
}

func (r *databaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database"
}

func (r *databaseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A tenant database backed by an lsm-rust instance, either packed onto a shared host or on a dedicated one. " +
			"Creation is asynchronous: the provider waits until the control plane reports the database as `ready`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Control-plane identifier of the database.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Human-readable database name. Can be changed in place.",
				Required:            true,
				Validators:          []validator.String{notBlank()},
			},
			"tier": schema.StringAttribute{
				MarkdownDescription: "Placement tier: `shared` or `dedicated`. Changing it destroys and recreates the database; in-place tier migration is not supported.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.OneOf("shared", "dedicated")},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Provisioning status reported by the control plane. Always `ready` after a successful apply; refresh warns if the control plane reports it as failed or in an unknown status.",
				Computed:            true,
			},
		},
		Blocks: map[string]schema.Block{
			"resource_limits": schema.SingleNestedBlock{
				MarkdownDescription: "Resource limits for the lsm-rust instance. Required.",
				Validators:          []validator.Object{objectvalidator.IsRequired()},
				Attributes: map[string]schema.Attribute{
					"memory_mb": schema.Int64Attribute{
						MarkdownDescription: "Memory limit in MiB.",
						Required:            true,
						Validators:          []validator.Int64{int64validator.AtLeast(1)},
					},
					"disk_gb": schema.Int64Attribute{
						MarkdownDescription: "Disk limit in GiB.",
						Required:            true,
						Validators:          []validator.Int64{int64validator.AtLeast(1)},
					},
				},
			},
			"backup_policy": schema.SingleNestedBlock{
				MarkdownDescription: "Backup schedule and retention. Required. Can be changed in place.",
				Validators:          []validator.Object{objectvalidator.IsRequired()},
				Attributes: map[string]schema.Attribute{
					"schedule": schema.StringAttribute{
						MarkdownDescription: "Cron expression for backups, e.g. `0 */6 * * *`.",
						Required:            true,
						Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
					},
					"retention": schema.StringAttribute{
						MarkdownDescription: "How long backups are kept, e.g. `14d`.",
						Required:            true,
						Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
					},
				},
			},
		},
	}
}

func (r *databaseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, errMsg := clientFromProviderData(req.ProviderData)
	if errMsg != "" {
		resp.Diagnostics.AddError("Unexpected provider data", errMsg)
		return
	}
	r.client = c
}

func (r *databaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan databaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	created, err := r.client.CreateDatabase(ctx, client.CreateDatabaseInput{
		Name:           plan.Name.ValueString(),
		Tier:           plan.Tier.ValueString(),
		ResourceLimits: plan.ResourceLimits.toAPI(),
		BackupPolicy:   plan.BackupPolicy.toAPI(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Error creating database", err.Error())
		return
	}
	if created.ID == "" {
		resp.Diagnostics.AddError("Error creating database", "The control plane response did not include a database id.")
		return
	}

	// Record the ID immediately so a failed wait leaves a tainted resource in
	// state rather than an orphan on the control plane.
	resp.Diagnostics.Append(resp.State.Set(ctx, databaseFromAPI(created))...)
	if resp.Diagnostics.HasError() {
		return
	}

	ready, err := r.waitReady(ctx, created.ID, func(*client.Database) bool { return true })
	if err != nil {
		resp.Diagnostics.AddError("Error waiting for database to become ready", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, databaseFromAPI(ready))...)
}

func (r *databaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state databaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	db, err := r.client.GetDatabase(ctx, state.ID.ValueString())
	if errors.Is(err, client.ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error reading database", err.Error())
		return
	}
	switch db.Status {
	case client.StatusReady, client.StatusPending, client.StatusUpdating:
	default:
		resp.Diagnostics.AddWarning("Database is unhealthy",
			fmt.Sprintf("The control plane reports database %s with status %q. If it does not recover, "+
				"recreate it with: terraform apply -replace=<resource address>", db.ID, db.Status))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, databaseFromAPI(db))...)
}

func (r *databaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state databaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := state.ID.ValueString()
	limits := plan.ResourceLimits.toAPI()
	var in client.UpdateDatabaseInput
	if !plan.Name.Equal(state.Name) {
		in.Name = plan.Name.ValueStringPointer()
	}
	if limits != state.ResourceLimits.toAPI() {
		in.ResourceLimits = &limits
	}
	if backup := plan.BackupPolicy.toAPI(); backup != state.BackupPolicy.toAPI() {
		in.BackupPolicy = &backup
	}

	if in != (client.UpdateDatabaseInput{}) {
		if err := r.client.UpdateDatabase(ctx, id, in); err != nil {
			resp.Diagnostics.AddError("Error updating database", err.Error())
			r.restoreState(ctx, id, state, resp)
			return
		}
	}

	ready, err := r.waitReady(ctx, id, func(db *client.Database) bool { return db.ResourceLimits == limits })
	if err != nil {
		resp.Diagnostics.AddError("Error waiting for database update to complete", err.Error())
		r.restoreState(ctx, id, state, resp)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, databaseFromAPI(ready))...)
}

// The framework pre-fills the update response with the plan, so on failure
// state must be reset explicitly or the unapplied values would be saved.
func (r *databaseResource) restoreState(ctx context.Context, id string, prior databaseModel, resp *resource.UpdateResponse) {
	if db, err := r.client.GetDatabase(ctx, id); err == nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, databaseFromAPI(db))...)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, prior)...)
}

func (r *databaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state databaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.DeleteDatabase(ctx, state.ID.ValueString())
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		resp.Diagnostics.AddError("Error deleting database", err.Error())
	}
}

func (r *databaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *databaseResource) waitReady(ctx context.Context, id string, done func(*client.Database) bool) (*client.Database, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseReadyTimeout)
	defer cancel()
	db, err := r.client.WaitForDatabase(ctx, id, databasePollInterval, done)
	if err != nil {
		return nil, fmt.Errorf("database %s: %w", id, err)
	}
	return db, nil
}

func databaseFromAPI(db *client.Database) databaseModel {
	return databaseModel{
		ID:     types.StringValue(db.ID),
		Name:   types.StringValue(db.Name),
		Tier:   types.StringValue(db.Tier),
		Status: types.StringValue(db.Status),
		ResourceLimits: &resourceLimitsModel{
			MemoryMB: types.Int64Value(db.ResourceLimits.MemoryMB),
			DiskGB:   types.Int64Value(db.ResourceLimits.DiskGB),
		},
		BackupPolicy: &backupPolicyModel{
			Schedule:  types.StringValue(db.BackupPolicy.Schedule),
			Retention: types.StringValue(db.BackupPolicy.Retention),
		},
	}
}

func (m *resourceLimitsModel) toAPI() client.ResourceLimits {
	if m == nil {
		return client.ResourceLimits{}
	}
	return client.ResourceLimits{MemoryMB: m.MemoryMB.ValueInt64(), DiskGB: m.DiskGB.ValueInt64()}
}

func (m *backupPolicyModel) toAPI() client.BackupPolicy {
	if m == nil {
		return client.BackupPolicy{}
	}
	return client.BackupPolicy{Schedule: m.Schedule.ValueString(), Retention: m.Retention.ValueString()}
}
