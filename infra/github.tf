# Let GitHub Actions deploy, without storing any Google key in GitHub.
#
# Workload Identity Federation: every GitHub Actions run can ask GitHub for a
# short-lived, signed identity token (OIDC) that says "I am a run of repo X, on
# branch Y". Google is set up below to trust GitHub's signature, check those
# claims, and swap the token for a short-lived Google access token for one
# service account. Nothing long-lived exists to leak or rotate.
#
#   GitHub run --(GitHub-signed token)--> Google STS --(1h token)--> act as github-deployer

locals {
  # Trust the repository's numeric IDs, not its name: if the repo were deleted
  # or renamed, someone else could create a repo with the same name, but never
  # with the same IDs. Look up with: gh api repos/msf143/language-classes --jq '.id, .owner.id'
  github_repo          = "msf143/language-classes"
  github_repo_id       = "1422089795"
  github_repo_owner_id = "103597798"
}

# A pool is a container for outside identity providers (GitHub here).
resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"

  depends_on = [google_project_service.api]
}

# The provider: trust tokens signed by GitHub Actions, and copy the claims we
# care about into attributes Google can check.
resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-actions"
  display_name                       = "GitHub Actions OIDC"

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }

  attribute_mapping = {
    "google.subject"           = "assertion.sub" # e.g. repo:msf143/language-classes:ref:refs/heads/main
    "attribute.repository"     = "assertion.repository"
    "attribute.repository_id"  = "assertion.repository_id"
    "attribute.repository_ref" = "assertion.ref"
  }

  # Reject every token except runs of our repository on the main branch.
  # (Pull requests and other branches can still run tests in GitHub, they just
  # can't get Google credentials.)
  attribute_condition = join(" && ", [
    "assertion.repository_id == '${local.github_repo_id}'",
    "assertion.repository_owner_id == '${local.github_repo_owner_id}'",
    "assertion.ref == 'refs/heads/main'",
  ])
}

# The identity deploys run as. Only the permissions below, nothing else.
resource "google_service_account" "deployer" {
  account_id   = "github-deployer"
  display_name = "GitHub Actions deployer"
}

# Who may act as the deployer: runs from our repository (which the provider's
# condition has already narrowed to main).
resource "google_service_account_iam_member" "deployer_from_github" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository_id/${local.github_repo_id}"
}

# ---------- what the deployer may do ----------

# Push images to our one repository.
resource "google_artifact_registry_repository_iam_member" "deployer_push" {
  location   = google_artifact_registry_repository.app.location
  repository = google_artifact_registry_repository.app.name
  role       = "roles/artifactregistry.writer"
  member     = google_service_account.deployer.member
}

# Deploy new revisions of Cloud Run services and run Cloud Run jobs (the
# migration job comes next). Can't change IAM or delete the project's other things.
resource "google_project_iam_member" "deployer_run" {
  project = local.project
  role    = "roles/run.developer"
  member  = google_service_account.deployer.member
}

# A Cloud Run revision runs as its environment's account (hablemos-api for
# prod); deploying one means "acting as" that account. Granted on those
# accounts only, not project-wide.
resource "google_service_account_iam_member" "deployer_acts_as_api" {
  for_each           = local.envs # on each environment's account, nothing else
  service_account_id = google_service_account.api[each.key].name
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.deployer.member
}

# The two values the workflow file needs. Neither is a secret.
output "github_workload_identity_provider" {
  value = google_iam_workload_identity_pool_provider.github.name
}

output "github_deployer_email" {
  value = google_service_account.deployer.email
}
