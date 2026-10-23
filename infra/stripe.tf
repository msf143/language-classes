# Stripe: two secrets per environment (cmd/api/stripe.go, cmd/api/webhooks.go).
#
#   stripe-secret-key*      the API key our server uses to call Stripe
#                           (create Checkout pages). A restricted key.
#   stripe-webhook-secret*  the signing secret (whsec_...) Stripe signs its
#                           webhook calls to us with; we check the HMAC.
#
# Each environment has its own pair, so staging's sandbox key can never reach
# production and the live key can never reach staging. Like the Resend key,
# Terraform creates the secrets empty and the values are added by hand
# (docs/deploy.md), so they're in no file, no git history and no state.
#
# The service reads them only when its env has `payments = true` (envs.tf,
# run.tf): Cloud Run won't start a revision that points at an empty secret.

resource "google_secret_manager_secret" "stripe_secret_key" {
  for_each  = local.envs
  secret_id = each.value.stripe_key_secret
  replication {
    auto {}
  }
  depends_on = [google_project_service.api]
}

resource "google_secret_manager_secret" "stripe_webhook_secret" {
  for_each  = local.envs
  secret_id = each.value.stripe_hook_secret
  replication {
    auto {}
  }
  depends_on = [google_project_service.api]
}

# Each environment's app may read its own two Stripe secrets, nobody else's.
resource "google_secret_manager_secret_iam_member" "api_stripe_secret_key" {
  for_each  = local.envs
  secret_id = google_secret_manager_secret.stripe_secret_key[each.key].id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.api[each.key].member
}

resource "google_secret_manager_secret_iam_member" "api_stripe_webhook_secret" {
  for_each  = local.envs
  secret_id = google_secret_manager_secret.stripe_webhook_secret[each.key].id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.api[each.key].member
}
