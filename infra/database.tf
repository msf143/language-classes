# Cloud SQL for Postgres, and the connection string the app reads as DATABASE_URL.
#
# How the app reaches the database: Cloud Run has a built-in Cloud SQL connector.
# It shows up inside the container as a Unix socket at /cloudsql/<connection name>,
# and only lets callers through whose service account has roles/cloudsql.client.
# The instance has a public IP (the connector uses it) but no authorized
# networks, so nothing on the internet can connect to it directly. No VPC needed.

resource "google_sql_database_instance" "main" {
  name             = "hablemos-pg"
  database_version = "POSTGRES_16" # same as the dev container
  region           = local.region

  settings {
    edition = "ENTERPRISE"  # the cheaper edition; needed for the shared-core tier
    tier    = "db-f1-micro" # shared CPU, 0.6 GB RAM: ~$8/month, plenty for now

    disk_type       = "PD_SSD"
    disk_size       = 10   # GB, the minimum
    disk_autoresize = true # grows (never shrinks) instead of filling up

    availability_type = "ZONAL" # one zone; "REGIONAL" doubles the price

    ip_configuration {
      ipv4_enabled = true
      ssl_mode     = "ENCRYPTED_ONLY"
      # No authorized_networks blocks: only the IAM-checked connector gets in.
    }

    backup_configuration {
      enabled    = true
      start_time = "07:00" # UTC = 2:00 in Bogotá
      backup_retention_settings {
        retained_backups = 7
      }
    }

    # Google refuses to delete the instance while this is on, whoever asks.
    deletion_protection_enabled = true
  }

  # Terraform refuses to destroy or replace it while this is on. Two locks,
  # because losing the database is the one mistake here that can't be undone.
  deletion_protection = true

  depends_on = [google_project_service.api]
}

resource "google_sql_database" "app" {
  name     = "app"
  instance = google_sql_database_instance.main.name
}

# ---------- the password ----------
#
# "ephemeral" means Terraform generates this value while it runs and never
# writes it to the state file. It can only be passed to "write-only" arguments
# (the *_wo ones below), which send it to Google and also aren't stored.
#
# A new random value is generated on every run, but write-only arguments are
# only sent when their *_wo_version changes. So the password is set once, and
# rotating it means bumping this number (both resources use it) and applying.
locals {
  db_password_version = 1
}

ephemeral "random_password" "db" {
  length  = 32
  special = false # letters and digits only: nothing to escape in the DSN
}

resource "google_sql_user" "app" {
  name                = "app"
  instance            = google_sql_database_instance.main.name
  password_wo         = ephemeral.random_password.db.result
  password_wo_version = local.db_password_version
}

# ---------- the secret ----------
#
# The whole connection string is the secret, so Cloud Run can hand it to the
# app as DATABASE_URL and main.go doesn't change. It's the keyword=value form
# of a Postgres connection string (pgx reads both forms). sslmode=disable is
# fine here: the socket only goes to the connector inside the container, and
# the connector encrypts everything it sends on to the database.
resource "google_secret_manager_secret" "database_url" {
  secret_id = "database-url"
  replication {
    auto {}
  }
  depends_on = [google_project_service.api]
}

resource "google_secret_manager_secret_version" "database_url" {
  secret                 = google_secret_manager_secret.database_url.id
  secret_data_wo_version = local.db_password_version
  secret_data_wo = join(" ", [
    "host=/cloudsql/${google_sql_database_instance.main.connection_name}",
    "dbname=${google_sql_database.app.name}",
    "user=${google_sql_user.app.name}",
    "password=${ephemeral.random_password.db.result}",
    "sslmode=disable",
  ])
}

# ---------- what the app's service account may do ----------

# Connect through the Cloud SQL connector.
resource "google_project_iam_member" "api_sql_client" {
  project = local.project
  role    = "roles/cloudsql.client"
  member  = google_service_account.api.member
}

# Read this one secret (not every secret in the project).
resource "google_secret_manager_secret_iam_member" "api_database_url" {
  secret_id = google_secret_manager_secret.database_url.id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.api.member
}

output "db_connection_name" {
  description = "PROJECT:REGION:INSTANCE, used by the Cloud SQL connector and Auth Proxy"
  value       = google_sql_database_instance.main.connection_name
}
