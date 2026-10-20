# Foundation: the settings everything else uses, the Google APIs we need, a
# place to store container images, and the identity the app runs as.

locals {
  project = "hablemos-live"
  region  = "us-east1"
}

# Turn on the Google services the app uses. bootstrap.sh already turned on the
# two Terraform needs to manage the project itself.
resource "google_project_service" "api" {
  for_each = toset([
    "artifactregistry.googleapis.com", # container images
    "run.googleapis.com",              # Cloud Run
    "sqladmin.googleapis.com",         # Cloud SQL
    "secretmanager.googleapis.com",    # DB password
    "siteverification.googleapis.com", # prove we own hablemos.live
    "sts.googleapis.com",              # swaps GitHub tokens for Google ones (github.tf)
    "iamcredentials.googleapis.com",   # short-lived service account tokens (github.tf)
  ])
  service = each.key

  # On `terraform destroy`, leave the API on. Turning an API off can break
  # things that weren't made by Terraform, and it costs nothing to leave on.
  disable_on_destroy = false
}

# A private Docker registry for the app's images, in the same region as Cloud
# Run (no cross-region pulls). Free up to 0.5 GB; our images are ~20 MB.
resource "google_artifact_registry_repository" "app" {
  repository_id = "app"
  location      = local.region
  format        = "DOCKER"
  description   = "Container images for the hablemos.live app"

  # Housekeeping so old images don't pile up past the free tier: keep the 10
  # newest, delete anything else older than 30 days.
  cleanup_policies {
    id     = "keep-newest-10"
    action = "KEEP"
    most_recent_versions {
      keep_count = 10
    }
  }
  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      older_than = "2592000s" # 30 days
    }
  }

  depends_on = [google_project_service.api]
}

# The identity the app runs as on Cloud Run. Without this, Cloud Run uses the
# project's default account, which has Editor on the whole project. With our
# own account, the app gets only the permissions granted to it below (and in
# later files): least privilege.
resource "google_service_account" "api" {
  for_each     = local.envs # one per environment (envs.tf)
  account_id   = each.value.service_account
  display_name = each.value.display_name
}

output "image_repo" {
  description = "Prefix for image names: docker push <this>/api:<tag>"
  value       = "${local.region}-docker.pkg.dev/${local.project}/${google_artifact_registry_repository.app.repository_id}"
}
