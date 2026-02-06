#!/bin/bash
# Setup script for Google Cloud service account with required permissions
#
# Usage:
#   ./setup-service-account.sh                    # Uses default 'ucp-service-account'
#   SERVICE_ACCOUNT_NAME=myaccount ./setup-service-account.sh  # Uses 'myaccount'

set -e

PROJECT_ID=$(gcloud config get-value project)
SERVICE_ACCOUNT_NAME="${SERVICE_ACCOUNT_NAME:-ucp-service-account}"
SERVICE_ACCOUNT_EMAIL="${SERVICE_ACCOUNT_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

echo "Setting up service account for project: ${PROJECT_ID}"

# Create service account if it doesn't exist
echo "Creating service account..."
gcloud iam service-accounts create ${SERVICE_ACCOUNT_NAME} \
  --display-name="UCP Service Account" \
  --description="Service account for UCP MCP and Chat servers" \
  2>/dev/null || echo "Service account already exists"

# Grant Secret Manager Secret Accessor role
echo "Granting Secret Manager access..."
gcloud projects add-iam-policy-binding ${PROJECT_ID} \
  --member="serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
  --role="roles/secretmanager.secretAccessor"

# Grant Cloud Run Invoker role (for service-to-service communication)
echo "Granting Cloud Run Invoker access..."
gcloud projects add-iam-policy-binding ${PROJECT_ID} \
  --member="serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
  --role="roles/run.invoker"

# Grant Cloud Trace Agent role (for logging/tracing)
echo "Granting Cloud Trace access..."
gcloud projects add-iam-policy-binding ${PROJECT_ID} \
  --member="serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
  --role="roles/cloudtrace.agent"

echo ""
echo "Service account setup complete: ${SERVICE_ACCOUNT_EMAIL}"
echo ""
echo "To deploy with this service account, add the following flag to your deployment command:"
echo "  --service-account ${SERVICE_ACCOUNT_EMAIL}"
