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
    # DNS records for hablemos.live (see domain.tf).
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
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

  # Charge API usage to our project. With a personal login, some APIs (Site
  # Verification) otherwise get charged to the project gcloud itself belongs
  # to, which hasn't turned them on: "requires a quota project". The identity
  # running Terraform needs serviceusage.services.use here (owners have it).
  user_project_override = true
  billing_project       = local.project
}

# Credentials come from the CLOUDFLARE_API_TOKEN environment variable (a
# Codespaces secret). The token may only edit DNS in the hablemos.live zone.
provider "cloudflare" {}
