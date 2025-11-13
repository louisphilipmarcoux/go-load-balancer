variable "project_id" {
  description = "Your GCP Project ID"
  type        = string
}

variable "region" {
  description = "The region to deploy in (e.g., us-central1)"
  type        = string
}

variable "zone" {
  description = "The zone to deploy in (e.g., us-central1-a)"
  type        = string
}

variable "credentials_file_path" {
  description = "Path to your GCP service account JSON key"
  type        = string
}

variable "ssh_public_key_path" {
  description = "Path to your local public SSH key"
  type        = string
}