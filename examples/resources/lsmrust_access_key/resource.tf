resource "lsmrust_access_key" "app" {
  database_id = lsmrust_database.orders.id
  scopes      = ["read", "write"]
}
