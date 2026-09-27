package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/zvdy/terraform-provider-lsmrust/internal/client"
)

func unitClient(t *testing.T, h http.HandlerFunc) *client.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, "", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// runUpdate mimics the framework: the response state starts as a copy of the plan.
func runUpdate(t *testing.T, r resource.Resource, prior, planned any) *resource.UpdateResponse {
	t.Helper()
	ctx := context.Background()
	var sr resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	null := tftypes.NewValue(sr.Schema.Type().TerraformType(ctx), nil)

	state := tfsdk.State{Schema: sr.Schema, Raw: null}
	if d := state.Set(ctx, prior); d.HasError() {
		t.Fatal(d)
	}
	plan := tfsdk.Plan{Schema: sr.Schema, Raw: null}
	if d := plan.Set(ctx, planned); d.HasError() {
		t.Fatal(d)
	}
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sr.Schema, Raw: plan.Raw.Copy()}}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, resp)
	return resp
}

var priorDB = client.Database{
	ID: "db-1", Name: "orders", Tier: "shared", Status: client.StatusReady,
	ResourceLimits: client.ResourceLimits{MemoryMB: 512, DiskGB: 10},
	BackupPolicy:   client.BackupPolicy{Schedule: "@daily", Retention: "14d"},
}

func plannedDB() databaseModel {
	m := databaseFromAPI(&priorDB)
	m.Status = types.StringUnknown()
	m.BackupPolicy.Retention = types.StringValue("30d")
	return m
}

func TestDatabaseUpdateFailureKeepsServerState(t *testing.T) {
	c := unitClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			writeJSONError(w, http.StatusServiceUnavailable, "try later")
			return
		}
		_ = json.NewEncoder(w).Encode(priorDB)
	})
	resp := runUpdate(t, &databaseResource{client: c}, databaseFromAPI(&priorDB), plannedDB())
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	var got databaseModel
	resp.State.Get(context.Background(), &got)
	if got.BackupPolicy.Retention.ValueString() != "14d" || got.Status.ValueString() != client.StatusReady {
		t.Fatalf("state after failed update = %+v / %+v, want the server's values", got, got.BackupPolicy)
	}
}

func TestDatabaseUpdateFailureFallsBackToPriorState(t *testing.T) {
	c := unitClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusBadRequest, "nope")
	})
	resp := runUpdate(t, &databaseResource{client: c}, databaseFromAPI(&priorDB), plannedDB())
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	var got databaseModel
	resp.State.Get(context.Background(), &got)
	if got.BackupPolicy.Retention.ValueString() != "14d" {
		t.Fatalf("retention = %s, want prior 14d", got.BackupPolicy.Retention)
	}
}

func TestAccessKeyUpdateFailureKeepsServerState(t *testing.T) {
	ctx := context.Background()
	c := unitClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			writeJSONError(w, http.StatusServiceUnavailable, "try later")
			return
		}
		_ = json.NewEncoder(w).Encode(client.AccessKey{ID: "key-1", DatabaseID: "db-1", Scopes: []string{"read", "write"}})
	})
	key := &client.AccessKey{ID: "key-1", DatabaseID: "db-1", Scopes: []string{"read", "write"}}
	prior, _ := accessKeyFromAPI(ctx, key, types.StringValue("s3cr3t"))
	planned := prior
	planned.Scopes, _ = types.SetValueFrom(ctx, types.StringType, []string{"read"})

	resp := runUpdate(t, &accessKeyResource{client: c}, prior, planned)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	var got accessKeyModel
	resp.State.Get(ctx, &got)
	if !got.Scopes.Equal(prior.Scopes) || got.Secret.ValueString() != "s3cr3t" {
		t.Fatalf("state after failed update = %+v, want scopes [read write] and the secret kept", got)
	}
}
