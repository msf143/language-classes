#!/usr/bin/env bash
# Runs once when the Codespace is created (or rebuilt). Installs the CLI tools
# this project uses on top of the Go base image.
set -euo pipefail

# Google Cloud CLI (gcloud): add Google's package repository, trusting only its signing key.
curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
  | sudo gpg --yes --dearmor -o /usr/share/keyrings/cloud.google.gpg
echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
  | sudo tee /etc/apt/sources.list.d/google-cloud-sdk.list > /dev/null

sudo apt-get update
sudo apt-get install -y postgresql-client jq google-cloud-cli

# Database migrations.
go install -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# Cloud SQL Auth Proxy: reach the prod database from here (see docs/deploy.md, "Running migrations").
go install github.com/GoogleCloudPlatform/cloud-sql-proxy/v2@latest

# Claude Code.
curl -fsSL https://claude.ai/install.sh | bash
