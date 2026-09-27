resource "lsmrust_database" "orders" {
  name = "orders-prod"
  tier = "dedicated"

  resource_limits {
    memory_mb = 4096
    disk_gb   = 100
  }

  backup_policy {
    schedule  = "0 */6 * * *"
    retention = "14d"
  }
}
