# The app itself: a Cloud Run service running our container image.
#
# Cloud Run takes an image and gives back an HTTPS URL. It starts copies of the
# container ("instances") when requests arrive and stops them when idle, so with
# min 0 instances we pay nothing while nobody is using the site. Google handles
# TLS in front; the Go server still speaks plain HTTP on $PORT.

locals {
  # Which image to run: a tag pushed to Artifact Registry (the git commit it was
  # built from). To deploy a new version for now: build + push a new tag, change
  # this, apply. GitHub Actions takes this job over later.
  image_tag = "d8c4617"
  image     = "${local.region}-docker.pkg.dev/${local.project}/${google_artifact_registry_repository.app.repository_id}/api:${local.image_tag}"

  # The site's public address; the app uses it to build login links. (The
  # service also answers at https://hablemos-api-529979780991.us-east1.run.app.)
  base_url = "https://${local.domain}"
}

resource "google_cloud_run_v2_service" "api" {
  name     = "hablemos-api"
  location = local.region
  ingress  = "INGRESS_TRAFFIC_ALL" # reachable from the internet

  # The service holds no data (that's in Cloud SQL), so Terraform may delete
  # or replace it freely. The database is the thing with deletion protection.
  deletion_protection = false

  template {
    # Run as our own identity, with only the permissions granted in database.tf.
    service_account = google_service_account.api.email

    scaling {
      min_instance_count = 0 # scale to zero when idle (cold start on the next request)
      max_instance_count = 2 # a cost and connection ceiling; plenty for now
    }

    # Mount the Cloud SQL connector. Inside the container it appears as a Unix
    # socket under /cloudsql/, which is the host= in DATABASE_URL.
    volumes {
      name = "cloudsql"
      cloud_sql_instance {
        instances = [google_sql_database_instance.main.connection_name]
      }
    }

    containers {
      image = local.image

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi" # the minimum allowed with 1 CPU; the app uses far less
        }
        cpu_idle          = true # CPU only while handling a request: billed per request
        startup_cpu_boost = true # extra CPU while starting, so cold starts are shorter
      }

      # Cloud Run reads the secret when an instance starts and hands it to the app
      # as an ordinary environment variable. Pinned to the version Terraform made,
      # so rotating the password (database.tf) also rolls out a new revision.
      env {
        name = "DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.database_url.secret_id
            version = google_secret_manager_secret_version.database_url.version
          }
        }
      }
      env {
        name  = "APP_BASE_URL"
        value = local.base_url
      }
      # No APP_ENV here: that's dev only. Without it and without an email provider,
      # POST /auth/login fails on purpose (see CLAUDE.md).

      volume_mounts {
        name       = "cloudsql"
        mount_path = "/cloudsql"
      }
    }
  }

  depends_on = [
    google_project_service.api,
    # The service account must be able to read the secret before the first
    # instance starts, or that start fails.
    google_secret_manager_secret_iam_member.api_database_url,
    google_project_iam_member.api_sql_client,
  ]
}

# The migration job: the same image, started with the argument "migrate", so it
# applies new migrations and exits instead of serving (cmd/api/migrate.go).
# A job runs to completion when executed, rather than waiting for requests:
#   gcloud run jobs execute migrate --region us-east1 --wait
# Deploys run it first, so new code never meets an old schema.
resource "google_cloud_run_v2_job" "migrate" {
  name                = "migrate"
  location            = local.region
  deletion_protection = false # holds no data

  template {
    task_count = 1 # one copy; the advisory lock would serialize extras anyway

    template {
      service_account = google_service_account.api.email # same permissions as the app
      max_retries     = 0                                # a failed migration needs a human, not a retry
      timeout         = "600s"

      volumes {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [google_sql_database_instance.main.connection_name]
        }
      }

      containers {
        image = local.image
        args  = ["migrate"]

        resources {
          limits = {
            cpu    = "1"
            memory = "512Mi"
          }
        }

        env {
          name = "DATABASE_URL"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.database_url.secret_id
              version = google_secret_manager_secret_version.database_url.version
            }
          }
        }

        volume_mounts {
          name       = "cloudsql"
          mount_path = "/cloudsql"
        }
      }
    }
  }

  depends_on = [
    google_project_service.api,
    google_secret_manager_secret_iam_member.api_database_url,
    google_project_iam_member.api_sql_client,
  ]
}

# Let anyone on the internet call the service (it's a public website). Without
# this, Cloud Run answers 403 to every request that isn't signed with a Google identity.
resource "google_cloud_run_v2_service_iam_member" "public" {
  name     = google_cloud_run_v2_service.api.name
  location = local.region
  role     = "roles/run.invoker"
  member   = "allUsers"
}

output "service_url" {
  description = "The app's Cloud Run address"
  value       = google_cloud_run_v2_service.api.uri
}
