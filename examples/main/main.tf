terraform {
  required_providers {
    lsmrust = {
      source = "zvdy/lsmrust"
    }
  }
}

provider "lsmrust" {}

variable "tier" {
  type    = string
  default = "dedicated"
}

variable "retention" {
  type    = string
  default = "14d"
}

resource "lsmrust_database" "orders" {
  name = "orders-prod"
  tier = var.tier

  resource_limits {
    memory_mb = 4096
    disk_gb   = 100
  }

  backup_policy {
    schedule  = "0 */6 * * *"
    retention = var.retention
  }
}

resource "lsmrust_access_key" "app" {
  database_id = lsmrust_database.orders.id
  scopes      = ["read", "write"]
}

output "database_id" {
  value = lsmrust_database.orders.id
}

output "database_status" {
  value = lsmrust_database.orders.status
}

output "access_key_secret" {
  value     = lsmrust_access_key.app.secret
  sensitive = true
}
