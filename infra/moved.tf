# Renames in Terraform's state, not changes in the cloud.
#
# When these resources became "one per environment" (for_each = local.envs),
# their addresses gained a key: google_cloud_run_v2_service.api became
# google_cloud_run_v2_service.api["prod"]. Without these blocks Terraform would
# read that as "destroy the old one, create a new one" (database included!).
# Each block says: same object, new address. The plan then shows "has moved"
# and changes nothing.
#
# Applied 2026-10-20. Safe to keep; they can be deleted once nobody has a
# state from before that date (there is only one state, so any time after).

moved {
  from = google_service_account.api
  to   = google_service_account.api["prod"]
}
moved {
  from = google_sql_database.app
  to   = google_sql_database.app["prod"]
}
moved {
  from = google_sql_user.app
  to   = google_sql_user.app["prod"]
}
moved {
  from = google_secret_manager_secret.database_url
  to   = google_secret_manager_secret.database_url["prod"]
}
moved {
  from = google_secret_manager_secret_version.database_url
  to   = google_secret_manager_secret_version.database_url["prod"]
}
moved {
  from = google_project_iam_member.api_sql_client
  to   = google_project_iam_member.api_sql_client["prod"]
}
moved {
  from = google_secret_manager_secret_iam_member.api_database_url
  to   = google_secret_manager_secret_iam_member.api_database_url["prod"]
}
moved {
  from = google_secret_manager_secret_iam_member.api_resend_api_key
  to   = google_secret_manager_secret_iam_member.api_resend_api_key["prod"]
}
moved {
  from = google_service_account_iam_member.deployer_acts_as_api
  to   = google_service_account_iam_member.deployer_acts_as_api["prod"]
}
moved {
  from = google_cloud_run_v2_service.api
  to   = google_cloud_run_v2_service.api["prod"]
}
moved {
  from = google_cloud_run_v2_job.migrate
  to   = google_cloud_run_v2_job.migrate["prod"]
}
moved {
  from = google_cloud_run_v2_service_iam_member.public
  to   = google_cloud_run_v2_service_iam_member.public["prod"]
}
