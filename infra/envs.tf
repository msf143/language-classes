# The environments: each gets its own database, user, secret, service account,
# Cloud Run service and migration job (resources with for_each = local.envs).
# Everything else is shared: the Cloud SQL instance, the image registry, the
# GitHub deployer, DNS, the Resend key. (Stripe keys are per environment:
# sandbox keys for staging, live keys for production.)
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
      job_args            = ["migrate"]             # what the migration job runs
      worker_job_name     = "worker"                # Cloud Run job for `api worker` (worker.tf)
      stripe_key_secret   = "stripe-secret-key"     # Stripe secrets, this env's own (stripe.tf)
      stripe_hook_secret  = "stripe-webhook-secret"
      payments            = true # live secrets added 2026-10-24; ON only for Mike's test purchase, then false until step 7
    }

    # https://dev.hablemos.live: a full copy for testing with fake money.
    # Its own database and identity; it can't read production's data or secret.
    staging = {
      service_name        = "hablemos-staging"
      job_name            = "migrate-staging"
      service_account     = "hablemos-staging"
      display_name        = "hablemos.live STAGING (Cloud Run)"
      db_name             = "staging"
      db_user             = "staging"
      db_password_version = 1
      secret_id           = "database-url-staging"
      base_url            = "https://dev.hablemos.live"
      app_env             = "staging"             # turns on the test banner, test credit, [TEST] emails
      job_args            = ["migrate", "--seed"] # migrations, then the seed data (refreshes class times)
      worker_job_name     = "worker-staging"
      stripe_key_secret   = "stripe-secret-key-staging" # sandbox keys: test cards, no real money
      stripe_hook_secret  = "stripe-webhook-secret-staging"
      payments            = true # sandbox secrets added 2026-10-24 (6e-2)
    }
  }
}
