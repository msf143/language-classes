# Receiving mail: hola@hablemos.live, forwarded by Cloudflare Email Routing to
# Mike's own inbox. Resend only sends (email.tf); this is the other direction,
# so students can write to us (questions, refunds of unused credit) and replies
# to our emails reach a person.
#
#   student ──> hola@hablemos.live ──(MX: Cloudflare)──> forwarded ──> Mike's inbox
#
# Where it forwards to is a personal address, so it's not in this repo: it comes
# from the Codespaces secret TF_VAR_HOLA_FORWARD_TO (Terraform reads an
# environment variable TF_VAR_<name> as the input variable <name>). It does end
# up in Terraform's state, which lives in the private state bucket.
#
# Why the variable's name is in capitals: GitHub stores secret names in upper
# case, whatever case you type, and Terraform matches the name exactly. A
# lower-case variable here would never see the secret.

variable "HOLA_FORWARD_TO" {
  description = "Mike's inbox that hola@hablemos.live forwards to (Codespaces secret TF_VAR_HOLA_FORWARD_TO)"
  type        = string
  sensitive   = true # never printed in plan/apply output
}

# The Cloudflare account the zone belongs to (destination addresses are kept per
# account, not per zone).
data "cloudflare_zone" "main" {
  zone_id = local.zone_id
}

# Turns Email Routing on for the zone. Cloudflare then adds and manages the
# records that make the internet deliver @hablemos.live mail to it: MX records
# on the bare domain and an SPF line. Resend's records are on the send.
# subdomain, so the two don't touch.
resource "cloudflare_email_routing_dns" "main" {
  zone_id = local.zone_id
}

# The destination. Cloudflare emails it once with a link; forwarding works only
# after that link is clicked (docs/deploy.md).
resource "cloudflare_email_routing_address" "mike" {
  account_id = data.cloudflare_zone.main.account.id
  email      = var.HOLA_FORWARD_TO
}

# hola@hablemos.live → the destination above. Other addresses at the domain are
# rejected (no catch-all), which keeps spam to random names out.
resource "cloudflare_email_routing_rule" "hola" {
  zone_id = local.zone_id
  name    = "hola@ to Mike"
  enabled = true
  matchers = [{
    type  = "literal"
    field = "to"
    value = "hola@${local.domain}"
  }]
  actions = [{
    type  = "forward"
    value = [cloudflare_email_routing_address.mike.email]
  }]
  depends_on = [cloudflare_email_routing_dns.main]
}
