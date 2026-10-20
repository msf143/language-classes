# The environments: each gets its own database, user, secret, service account,
# Cloud Run service and migration job (resources with for_each = local.envs).
# Everything else is shared: the Cloud SQL instance, the image registry, the
# GitHub deployer, DNS, the Resend key.
#
# Adding an environment = adding an entry here (plus its domain in domain.tf).

locals {
  envs = {
    prod = {
      service_name        = "hablemos-api"                  # Cloud Run service
      job_name            = "migrate"                       # Cloud Run migration job
      service_account     = "hablemos-api"                  # identity the app runs as
      display_name        = "hablemos.live API (Cloud Run)" # the service account's label
      db_name             = "app"                           # database on the shared instance
      db_user             = "app"
      db_password_version = 1                       # bump to rotate this env's password
      secret_id           = "database-url"          # its DATABASE_URL
      base_url            = "https://hablemos.live" # used in login links
      app_env             = ""                      # APP_ENV; "" = production (not set)
    }
  }
}
