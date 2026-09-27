package provider

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/zvdy/terraform-provider-lsmrust/internal/client"
	"github.com/zvdy/terraform-provider-lsmrust/internal/mockserver"
)

const testAPIKey = "acc-test-key"

var (
	testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
		"lsmrust": providerserver.NewProtocol6WithError(New("test")()),
	}

	testClient   *client.Client
	testEndpoint string
)

// TF_ACC defaults to on because the backend is a free in-process mock; TF_ACC=0 skips.
func TestMain(m *testing.M) {
	srv := httptest.NewServer(mockserver.New(mockserver.Config{
		ProvisionDelay: 2 * time.Second,
		APIKey:         testAPIKey,
	}))

	if _, ok := os.LookupEnv("TF_ACC"); !ok {
		os.Setenv("TF_ACC", "1")
	} else if os.Getenv("TF_ACC") == "0" {
		os.Unsetenv("TF_ACC")
	}
	testEndpoint = srv.URL
	os.Setenv(envEndpoint, srv.URL)
	os.Setenv(envAPIKey, testAPIKey)

	var err error
	testClient, err = client.New(srv.URL, testAPIKey, nil)
	if err != nil {
		log.Fatal(err)
	}

	code := m.Run()
	srv.Close()
	os.Exit(code)
}

func checkDestroyed(resourceType string, get func(ctx context.Context, id string) error) func(*terraform.State) error {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != resourceType {
				continue
			}
			err := get(context.Background(), rs.Primary.ID)
			if err == nil {
				return fmt.Errorf("%s %s still exists on the control plane", resourceType, rs.Primary.ID)
			}
			if !errors.Is(err, client.ErrNotFound) {
				return err
			}
		}
		return nil
	}
}

func testAccCheckDatabaseDestroy(s *terraform.State) error {
	return checkDestroyed("lsmrust_database", func(ctx context.Context, id string) error {
		_, err := testClient.GetDatabase(ctx, id)
		return err
	})(s)
}

func testAccCheckAccessKeyDestroy(s *terraform.State) error {
	return checkDestroyed("lsmrust_access_key", func(ctx context.Context, id string) error {
		_, err := testClient.GetAccessKey(ctx, id)
		return err
	})(s)
}

func captureAttr(name, attr string, dst *string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("resource %s not found in state", name)
		}
		*dst = rs.Primary.Attributes[attr]
		if *dst == "" {
			return fmt.Errorf("%s.%s is empty", name, attr)
		}
		return nil
	}
}

func expectAttr(name, attr string, want *string, same bool) func(*terraform.State) error {
	return func(s *terraform.State) error {
		got := s.RootModule().Resources[name].Primary.Attributes[attr]
		if same && got != *want {
			return fmt.Errorf("%s.%s changed from %q to %q; expected it to be preserved", name, attr, *want, got)
		}
		if !same && got == *want {
			return fmt.Errorf("%s.%s is still %q; expected a new value", name, attr, got)
		}
		return nil
	}
}
