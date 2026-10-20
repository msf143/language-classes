# https://hablemos.live -> the Cloud Run service.
#
# Three parties are involved:
#   Cloudflare  holds the DNS zone (where the name points). Records are "DNS only"
#               (grey cloud): browsers connect straight to Google, not via Cloudflare.
#   Google Site Verification  proves we own the domain, using a TXT record.
#               Google won't attach a domain to Cloud Run until it's verified.
#   Cloud Run domain mapping  tells Google's front end that requests for
#               hablemos.live go to our service, and makes Google issue and renew
#               an HTTPS certificate for it (free, automatic, takes 15 min to hours).

locals {
  domain  = "hablemos.live"
  zone_id = "0a909059c691a0ff1b1f2f1eca481b2e" # Cloudflare zone for hablemos.live
}

# ---------- 1. prove we own the domain ----------

# Ask Google for a verification string to publish in DNS. Needs the extra
# "siteverification" scope on the application-default login (see docs/deploy.md).
data "google_site_verification_token" "domain" {
  type                = "INET_DOMAIN"
  identifier          = local.domain
  verification_method = "DNS_TXT"
  # Read while planning, so the Site Verification API must already be on:
  # bootstrap.sh turns it on (main.tf also lists it).
}

resource "cloudflare_dns_record" "google_verification" {
  zone_id = local.zone_id
  name    = local.domain
  type    = "TXT"
  content = "\"${data.google_site_verification_token.domain.token}\""
  ttl     = 1 # 1 = "automatic" in Cloudflare
  comment = "Google site verification (Terraform)"
}

# Tell Google to check the TXT record. On success, the account Terraform runs as
# (yours) becomes a verified owner of hablemos.live.
resource "google_site_verification_web_resource" "domain" {
  site {
    type       = data.google_site_verification_token.domain.type
    identifier = data.google_site_verification_token.domain.identifier
  }
  verification_method = data.google_site_verification_token.domain.verification_method

  depends_on = [cloudflare_dns_record.google_verification, google_project_service.api]
}

# ---------- 2. attach the domain to the service ----------

# Domain mappings exist only in Cloud Run's older (v1) API, so this resource
# looks a little different from run.tf.
resource "google_cloud_run_domain_mapping" "apex" {
  name     = local.domain
  location = local.region
  metadata {
    namespace = local.project
  }
  spec {
    route_name = google_cloud_run_v2_service.api["prod"].name
  }

  depends_on = [google_site_verification_web_resource.domain]
}

# ---------- 3. point the name at Google ----------

# A bare domain (no "www.") can't be a CNAME, so it gets address records.
# These are the fixed addresses Google documents for every Cloud Run domain
# mapping of an apex domain; the output below shows what the mapping itself
# asks for, so the two can be compared after apply.
locals {
  google_apex_a    = ["216.239.32.21", "216.239.34.21", "216.239.36.21", "216.239.38.21"]
  google_apex_aaaa = ["2001:4860:4802:32::15", "2001:4860:4802:34::15", "2001:4860:4802:36::15", "2001:4860:4802:38::15"]
}

resource "cloudflare_dns_record" "apex_a" {
  for_each = toset(local.google_apex_a)
  zone_id  = local.zone_id
  name     = local.domain
  type     = "A"
  content  = each.key
  ttl      = 1
  proxied  = false # grey cloud: DNS only
  comment  = "Cloud Run domain mapping (Terraform)"
}

resource "cloudflare_dns_record" "apex_aaaa" {
  for_each = toset(local.google_apex_aaaa)
  zone_id  = local.zone_id
  name     = local.domain
  type     = "AAAA"
  content  = each.key
  ttl      = 1
  proxied  = false
  comment  = "Cloud Run domain mapping (Terraform)"
}

# ---------- 4. www.hablemos.live -> hablemos.live ----------

# www is mapped to the same service; the app itself answers with a redirect to
# the bare domain (redirectWWW in cmd/api/main.go). Google needs the mapping
# so it accepts www requests and issues a certificate for that name too.
resource "google_cloud_run_domain_mapping" "www" {
  name     = "www.${local.domain}"
  location = local.region
  metadata {
    namespace = local.project
  }
  spec {
    route_name = google_cloud_run_v2_service.api["prod"].name
  }

  depends_on = [google_site_verification_web_resource.domain]
}

# A subdomain can be a CNAME (an alias for another name). Google's front end
# for custom domains is ghs.googlehosted.com.
resource "cloudflare_dns_record" "www" {
  zone_id = local.zone_id
  name    = "www.${local.domain}"
  type    = "CNAME"
  content = "ghs.googlehosted.com"
  ttl     = 1
  proxied = false
  comment = "Cloud Run domain mapping (Terraform)"
}

output "domain_mapping_records" {
  description = "DNS records the domain mapping asks for (should match the A/AAAA records above)"
  value       = google_cloud_run_domain_mapping.apex.status[0].resource_records
}

# ---------- 5. dev.hablemos.live -> the staging service ----------

# Same pattern as www: a domain mapping (so Google accepts the name and issues
# a certificate) and a CNAME to Google's front end. Subdomains of a verified
# domain need no verification of their own.
resource "google_cloud_run_domain_mapping" "dev" {
  name     = "dev.${local.domain}"
  location = local.region
  metadata {
    namespace = local.project
  }
  spec {
    route_name = google_cloud_run_v2_service.api["staging"].name
  }

  depends_on = [google_site_verification_web_resource.domain]
}

resource "cloudflare_dns_record" "dev" {
  zone_id = local.zone_id
  name    = "dev.${local.domain}"
  type    = "CNAME"
  content = "ghs.googlehosted.com"
  ttl     = 1
  proxied = false
  comment = "Staging: Cloud Run domain mapping (Terraform)"
}
