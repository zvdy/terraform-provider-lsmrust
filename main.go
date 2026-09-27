package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/zvdy/terraform-provider-lsmrust/internal/provider"
)

//go:generate go tool tfplugindocs generate --provider-name lsmrust

var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/zvdy/lsmrust",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
