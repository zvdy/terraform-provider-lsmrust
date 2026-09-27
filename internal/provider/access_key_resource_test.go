package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testKey = "lsmrust_access_key.test"

func testAccAccessKeyConfig(dbRef string, scopes string) string {
	return fmt.Sprintf(`
resource "lsmrust_database" "a" {
  name = "keys-acc-a"
  tier = "shared"
  resource_limits {
    memory_mb = 256
    disk_gb   = 5
  }
  backup_policy {
    schedule  = "@daily"
    retention = "7d"
  }
}

resource "lsmrust_database" "b" {
  name = "keys-acc-b"
  tier = "shared"
  resource_limits {
    memory_mb = 256
    disk_gb   = 5
  }
  backup_policy {
    schedule  = "@daily"
    retention = "7d"
  }
}

resource "lsmrust_access_key" "test" {
  database_id = lsmrust_database.%s.id
  scopes      = %s
}
`, dbRef, scopes)
}

func TestAccAccessKey_lifecycle(t *testing.T) {
	var keyID, secret string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			if err := testAccCheckAccessKeyDestroy(s); err != nil {
				return err
			}
			return testAccCheckDatabaseDestroy(s)
		},
		Steps: []resource.TestStep{
			{
				Config: testAccAccessKeyConfig("a", `["read", "write"]`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testKey, tfjsonpath.New("scopes"), knownvalue.SetExact([]knownvalue.Check{
						knownvalue.StringExact("read"), knownvalue.StringExact("write"),
					})),
					statecheck.ExpectKnownValue(testKey, tfjsonpath.New("secret"), knownvalue.StringRegexp(regexp.MustCompile(`^[0-9a-f]{64}$`))),
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair(testKey, "database_id", "lsmrust_database.a", "id"),
					captureAttr(testKey, "id", &keyID),
					captureAttr(testKey, "secret", &secret),
				),
			},
			{
				Config:   testAccAccessKeyConfig("a", `["write", "read"]`),
				PlanOnly: true,
			},
			{
				Config: testAccAccessKeyConfig("a", `["read"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testKey, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					expectAttr(testKey, "id", &keyID, true),
					expectAttr(testKey, "secret", &secret, true),
					func(s *terraform.State) error {
						k, err := testClient.GetAccessKey(context.Background(), keyID)
						if err != nil {
							return err
						}
						if !slices.Equal(k.Scopes, []string{"read"}) {
							return fmt.Errorf("server scopes = %v, want [read]", k.Scopes)
						}
						return nil
					},
				),
			},
			{
				RefreshState: true,
				Check:        expectAttr(testKey, "secret", &secret, true),
			},
			{
				ResourceName:            testKey,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
			{
				Config: testAccAccessKeyConfig("b", `["read"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testKey, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrPair(testKey, "database_id", "lsmrust_database.b", "id"),
					expectAttr(testKey, "id", &keyID, false),
					expectAttr(testKey, "secret", &secret, false),
				),
			},
		},
	})
}

func TestAccAccessKey_validation(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccAccessKeyConfig("a", `["read", "superuser"]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`value must be one of`),
			},
			{
				Config: `
resource "lsmrust_access_key" "test" {
  database_id = ""
  scopes      = ["read"]
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must not be empty or only whitespace`),
			},
			{
				Config:      testAccAccessKeyConfig("a", `[]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`at least 1`),
			},
		},
	})
}
