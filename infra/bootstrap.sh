#!/usr/bin/env bash
# One-time setup that has to exist before Terraform can run (the chicken-and-egg part).
# Everything else in Google Cloud is created by the Terraform files in this folder.
# Safe to rerun: each step skips work that's already done.
#
#   bash infra/bootstrap.sh
set -euo pipefail

PROJECT=hablemos-live
REGION=us-east1
STATE_BUCKET="gs://${PROJECT}-tfstate"

# APIs Terraform itself needs before it can plan. Terraform turns on the rest
# (Cloud Run, Cloud SQL, ...) so that list lives in one place. Site Verification
# is here because domain.tf reads a token from it while planning.
gcloud services enable --project "$PROJECT" \
  cloudresourcemanager.googleapis.com \
  iam.googleapis.com \
  siteverification.googleapis.com

# Bucket for Terraform's state file (its record of what it created).
# Versioning keeps every previous state, so a bad apply can be recovered.
if ! gcloud storage buckets describe "$STATE_BUCKET" --project "$PROJECT" > /dev/null 2>&1; then
  gcloud storage buckets create "$STATE_BUCKET" \
    --project "$PROJECT" \
    --location "$REGION" \
    --uniform-bucket-level-access \
    --public-access-prevention
fi
gcloud storage buckets update "$STATE_BUCKET" --versioning

echo "Bootstrap done: APIs enabled, state bucket $STATE_BUCKET ready."
