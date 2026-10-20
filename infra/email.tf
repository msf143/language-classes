# DNS records that let Resend send email "from" hablemos.live (login links).
#
# Receiving mail servers (Gmail, Outlook, ...) look these up for every message:
#   DKIM   a public key. Resend signs each email with the private half; receivers
#          check the signature, proving the mail came from our domain unaltered.
#   send / rsend  CNAMEs delegating the sending subdomains to Resend, which
#          publishes the SPF ("these servers may send for us") and bounce-handling
#          records there, and can change them without us touching DNS.
#   DMARC  what receivers should do when those checks fail. p=none = report only
#          for now; tighten to quarantine/reject once mail is flowing cleanly.
#
# Values come from Resend: Domains > hablemos.live > Records. All are public.
# The domain is verified there (Verify button) after these records exist.

locals {
  # The DKIM public key exactly as Resend shows it (starts with p=, ends with IDAQAB).
  resend_dkim = "p=MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDWW+Ajr6zh6DK8G+9RFFKyjiegZ0mQGQ/fmSGYBVC0inpJ0BXlaImJbUSZDm6QntWAd8RrhEAL8L4SpK19274756Mj4h9Rff4SlWEzMH1FwHQDQIbDv0FtuiqXRP4bNq/t9tAJWK79l/3sOokrq9+J9v5hcHn+D265xjO8GVMa3wIDAQAB"
}

resource "cloudflare_dns_record" "resend_dkim" {
  zone_id = local.zone_id
  name    = "resend._domainkey.${local.domain}"
  type    = "TXT"
  content = "\"${local.resend_dkim}\""
  ttl     = 1
  comment = "Resend DKIM (Terraform)"
}

resource "cloudflare_dns_record" "resend_cname" {
  for_each = {
    send  = "send.forge.rmta.net"
    rsend = "rsend.forge.rmta.net"
  }
  zone_id = local.zone_id
  name    = "${each.key}.${local.domain}"
  type    = "CNAME"
  content = each.value
  ttl     = 1
  proxied = false # DNS only, as Resend requires
  comment = "Resend sending (Terraform)"
}

# ---------- the Resend API key ----------
#
# Terraform creates the secret but never sees its value: Mike adds it with
#   gcloud secrets versions add resend-api-key --data-file=-
# (see docs/deploy.md). So the key is in no file, no git history, no state.
resource "google_secret_manager_secret" "resend_api_key" {
  secret_id = "resend-api-key"
  replication {
    auto {}
  }
  depends_on = [google_project_service.api]
}

# Each environment's app may read this one secret (all environments share the key).
resource "google_secret_manager_secret_iam_member" "api_resend_api_key" {
  for_each  = local.envs
  secret_id = google_secret_manager_secret.resend_api_key.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.api[each.key].member
}

resource "cloudflare_dns_record" "dmarc" {
  zone_id = local.zone_id
  name    = "_dmarc.${local.domain}"
  type    = "TXT"
  content = "\"v=DMARC1; p=none;\""
  ttl     = 1
  comment = "DMARC policy (Terraform)"
}
