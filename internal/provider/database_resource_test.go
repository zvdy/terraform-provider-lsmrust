package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/zvdy/terraform-provider-lsmrust/internal/client"
)

const testDB = "lsmrust_database.test"

func testAccDatabaseConfig(name, tier string, memoryMB int, retention string) string {
	return fmt.Sprintf(`
resource "lsmrust_database" "test" {
  name = %q
  tier = %q

  resource_limits {
    memory_mb = %d
    disk_gb   = 20
  }

  backup_policy {
    schedule  = "0 */6 * * *"
    retention = %q
  }
}
`, name, tier, memoryMB, retention)
}

func TestAccDatabase_lifecycle(t *testing.T) {
	var firstID, currentID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccDatabaseConfig("orders-acc", "shared", 1024, "14d"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("name"), knownvalue.StringExact("orders-acc")),
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("tier"), knownvalue.StringExact("shared")),
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("status"), knownvalue.StringExact("ready")),
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("resource_limits"), knownvalue.ObjectExact(map[string]knownvalue.Check{
						"memory_mb": knownvalue.Int64Exact(1024),
						"disk_gb":   knownvalue.Int64Exact(20),
					})),
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("backup_policy"), knownvalue.ObjectExact(map[string]knownvalue.Check{
						"schedule":  knownvalue.StringExact("0 */6 * * *"),
						"retention": knownvalue.StringExact("14d"),
					})),
				},
				Check: resource.ComposeTestCheckFunc(
					captureAttr(testDB, "id", &firstID),
					testAccCheckDatabaseOnServer(testDB, func(db *client.Database) error {
						if db.Status != client.StatusReady || db.BackupPolicy.Retention != "14d" {
							return fmt.Errorf("server state mismatch: %+v", db)
						}
						return nil
					}),
				),
			},
			{
				Config: testAccDatabaseConfig("orders-acc", "shared", 1024, "30d"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDB, plancheck.ResourceActionUpdate),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("backup_policy").AtMapKey("retention"), knownvalue.StringExact("30d")),
				},
				Check: resource.ComposeTestCheckFunc(
					expectAttr(testDB, "id", &firstID, true),
					testAccCheckDatabaseOnServer(testDB, func(db *client.Database) error {
						if db.BackupPolicy.Retention != "30d" {
							return fmt.Errorf("server retention = %q, want 30d", db.BackupPolicy.Retention)
						}
						return nil
					}),
				),
			},
			{
				Config: testAccDatabaseConfig("orders-acc-renamed", "shared", 2048, "30d"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDB, plancheck.ResourceActionUpdate),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("resource_limits").AtMapKey("memory_mb"), knownvalue.Int64Exact(2048)),
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("name"), knownvalue.StringExact("orders-acc-renamed")),
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("status"), knownvalue.StringExact("ready")),
				},
				Check: expectAttr(testDB, "id", &firstID, true),
			},
			{
				Config: testAccDatabaseConfig("orders-acc-renamed", "dedicated", 2048, "30d"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDB, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("tier"), knownvalue.StringExact("dedicated")),
					statecheck.ExpectKnownValue(testDB, tfjsonpath.New("status"), knownvalue.StringExact("ready")),
				},
				Check: resource.ComposeTestCheckFunc(
					expectAttr(testDB, "id", &firstID, false),
					captureAttr(testDB, "id", &currentID),
					func(*terraform.State) error {
						_, err := testClient.GetDatabase(context.Background(), firstID)
						if !errors.Is(err, client.ErrNotFound) {
							return fmt.Errorf("replaced database %s should be gone, got err=%v", firstID, err)
						}
						return nil
					},
				),
			},
			{
				ResourceName:      testDB,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccDatabase_disappears(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckDatabaseDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccDatabaseConfig("gone-acc", "shared", 512, "7d"),
				Check: func(s *terraform.State) error {
					return testClient.DeleteDatabase(context.Background(), s.RootModule().Resources[testDB].Primary.ID)
				},
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccDatabase_wrongEndpointKeepsState(t *testing.T) {
	config := testAccDatabaseConfig("wrong-endpoint-acc", "shared", 512, "7d")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckDatabaseDestroy,
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:      fmt.Sprintf("provider \"lsmrust\" {\n  endpoint = %q\n}\n", testEndpoint+"/wrong-base") + config,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`HTTP 404`),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestAccProvider_emptyEndpointFallsBackToEnv(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:             "provider \"lsmrust\" {\n  endpoint = \"\"\n  api_key  = \"\"\n}\n" + testAccDatabaseConfig("env-fallback-acc", "shared", 512, "7d"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccDatabase_validation(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccDatabaseConfig("bad", "enormous", 512, "7d"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)value must be one of.*shared.*dedicated`),
			},
			{
				Config: `
resource "lsmrust_database" "test" {
  name = "no-blocks"
  tier = "shared"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Block resource_limits must have a configuration value`),
			},
			{
				Config:      testAccDatabaseConfig("   ", "shared", 512, "7d"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must not be empty or only whitespace`),
			},
			{
				Config:      testAccDatabaseConfig("bad", "shared", 0, "7d"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must be at least 1`),
			},
		},
	})
}

func testAccCheckDatabaseOnServer(name string, check func(*client.Database) error) func(*terraform.State) error {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("resource %s not found in state", name)
		}
		db, err := testClient.GetDatabase(context.Background(), rs.Primary.ID)
		if err != nil {
			return err
		}
		return check(db)
	}
}
