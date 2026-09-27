provider "lsmrust" {
  endpoint = "http://localhost:8080"
  api_key  = var.lsmrust_api_key
}

variable "lsmrust_api_key" {
  type      = string
  sensitive = true
}
