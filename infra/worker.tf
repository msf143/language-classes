# The worker: `api worker` (cmd/api/worker.go) run as a Cloud Run job, started
# every minute by Cloud Scheduler. Each run sends due emails (booking
# confirmations, cancellations, reminders), decides group classes 24 hours
# before they start, and purges old sign-ins, then exits. Nothing runs (or
# costs anything) between runs.
#
#   Cloud Scheduler --(every minute, as scheduler-invoker)--> Run Admin API: jobs.run
#     --> a worker execution (as the environment's app account) --> Cloud SQL, Resend

# One worker job per environment, built like the migration job: same image,
# same identity, same database socket and secret; plus the Resend key (it sends
# email) and the site address (links in emails).
resource "google_cloud_run_v2_job" "worker" {
  for_each            = local.envs
  name                = each.value.worker_job_name
  location            = local.region
  deletion_protection = false # holds no data

  template {
    task_count = 1

    template {
      service_account = google_service_account.api[each.key].email
      max_retries     = 0      # a failed run is simply followed by the next minute's run
      timeout         = "120s" # a run stops itself after 50 s; this is a safety net

      volumes {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [google_sql_database_instance.main.connection_name]
        }
      }

      containers {
        image = local.image
        args  = ["worker"]

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
              secret  = google_secret_manager_secret.database_url[each.key].secret_id
              version = google_secret_manager_secret_version.database_url[each.key].version
            }
          }
        }
        env {
          name = "RESEND_API_KEY"
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.resend_api_key.secret_id
              version = "latest"
            }
          }
        }
        env {
          name  = "APP_BASE_URL" # "Your bookings: https://hablemos.live" in emails
          value = each.value.base_url
        }
        dynamic "env" {
          for_each = each.value.app_env == "" ? [] : [each.value.app_env]
          content {
            name  = "APP_ENV" # staging: [TEST] in subjects
            value = env.value
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
    google_secret_manager_secret_iam_member.api_resend_api_key,
    google_project_iam_member.api_sql_client,
  ]

  # GitHub Actions sets the image on each deploy, like the other jobs.
  lifecycle {
    ignore_changes = [template[0].template[0].containers[0].image, client, client_version]
  }
}

# The identity Cloud Scheduler uses to start the worker jobs. It may do exactly
# that, on these jobs only: nothing else in the project.
resource "google_service_account" "scheduler" {
  account_id   = "scheduler-invoker"
  display_name = "Cloud Scheduler: starts the worker jobs"
}

resource "google_cloud_run_v2_job_iam_member" "scheduler_runs_worker" {
  for_each = local.envs
  name     = google_cloud_run_v2_job.worker[each.key].name
  location = local.region
  role     = "roles/run.invoker" # includes run.jobs.run: start an execution
  member   = google_service_account.scheduler.member
}

# Every minute (cron's finest grain), ask the Cloud Run Admin API to run the
# job. The request carries a Google access token for scheduler-invoker.
resource "google_cloud_scheduler_job" "worker" {
  for_each  = local.envs
  name      = "${each.value.worker_job_name}-every-minute"
  region    = local.region
  schedule  = "* * * * *"
  time_zone = "Etc/UTC"

  # A missed minute is fine: the next run picks up everything still due.
  retry_config {
    retry_count = 0
  }

  http_target {
    http_method = "POST"
    uri         = "https://run.googleapis.com/v2/projects/${local.project}/locations/${local.region}/jobs/${google_cloud_run_v2_job.worker[each.key].name}:run"
    oauth_token {
      service_account_email = google_service_account.scheduler.email
      scope                 = "https://www.googleapis.com/auth/cloud-platform"
    }
  }

  depends_on = [google_project_service.api, google_cloud_run_v2_job_iam_member.scheduler_runs_worker]
}
