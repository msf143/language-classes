# Which Terraform and providers this folder needs, and where the state lives.
#
# "State" is Terraform's record of what it created and the IDs Google gave each
# thing. On every plan it compares three things: these files (what we want), the
# state (what it thinks exists) and the real cloud (what actually exists).
# Keeping the state in a bucket (made by bootstrap.sh) instead of on this disk
# means a Codespace rebuild loses nothing, and the bucket's versioning keeps
# every old copy.

terraform {
  required_version = ">= 1.11"

  backend "gcs" {
    bucket = "hablemos-live-tfstate"
    prefix = "main"
  }

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
    # Generates the database password (see database.tf).
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }
}

# Credentials come from `gcloud auth application-default login`.
provider "google" {
  project = local.project
  region  = local.region
}
