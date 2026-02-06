.PHONY: build build-mcp build-chat push push-mcp push-chat deploy deploy-mcp deploy-chat local clean

# Variables
PROJECT_ID ?= $(shell gcloud config get-value project)
REGION ?= us-east1
SERVICE_ACCOUNT ?= $(SERVICE_ACCOUNT_NAME)
MCP_IMAGE = gcr.io/$(PROJECT_ID)/ucp-mcp-server
CHAT_IMAGE = gcr.io/$(PROJECT_ID)/ucp-chat-server

# Build commands
build: build-mcp build-chat

build-mcp:
	docker build -f Dockerfile.mcp -t $(MCP_IMAGE):latest .

build-chat:
	docker build -f Dockerfile.chat -t $(CHAT_IMAGE):latest .

# Push commands
push: push-mcp push-chat

push-mcp:
	docker push $(MCP_IMAGE):latest

push-chat:
	docker push $(CHAT_IMAGE):latest

# Local development
local:
	docker-compose up --build

local-down:
	docker-compose down

# Deploy to Google Cloud Run
deploy: deploy-mcp deploy-chat

deploy-mcp:
	gcloud run deploy ucp-mcp-server \
		--image $(MCP_IMAGE):latest \
		--region $(REGION) \
		--platform managed \
		--port 8080 \
		--allow-unauthenticated \
		--service-account $(SERVICE_ACCOUNT)@$(PROJECT_ID).iam.gserviceaccount.com \
		--set-env-vars "UCP_REGION=ZA" \
		--set-secrets "SHOPIFY_ACCESS_TOKEN=shopify-access-token:latest,OPENAI_API_KEY=openai-api-key:latest" \
		--memory 512Mi \
		--cpu 1

deploy-chat:
	$(eval MCP_URL := $(shell gcloud run services describe ucp-mcp-server --region $(REGION) --format 'value(status.url)'))
	gcloud run deploy ucp-chat-server \
		--image $(CHAT_IMAGE):latest \
		--region $(REGION) \
		--platform managed \
		--port 8090 \
		--allow-unauthenticated \
		--service-account $(SERVICE_ACCOUNT)@$(PROJECT_ID).iam.gserviceaccount.com \
		--set-env-vars "CHAT_SERVER_ADDR=:8090,CHAT_DB_PATH=/tmp/chat_history.db,MCP_SERVER_URL=$(MCP_URL)" \
		--set-secrets "OPENAI_API_KEY=openai-api-key:latest,TWILIO_ACCOUNT_SID=twilio-account-sid:latest,TWILIO_AUTH_TOKEN=twilio-auth-token:latest,TWILIO_PHONE_NUMBER=twilio-phone-number:latest" \
		--memory 512Mi \
		--cpu 1

# Cloud Build deployment (simple version - no git required)
deploy-cloud-build:
	gcloud builds submit --config cloudbuild.simple.yaml

# Cloud Build deployment (with git commit SHA - requires git trigger)
deploy-cloud-build-git:
	gcloud builds submit --config cloudbuild.yaml

# View logs
logs-mcp:
	gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=ucp-mcp-server" --limit=50

logs-chat:
	gcloud logging read "resource.type=cloud_run_revision AND resource.labels.service_name=ucp-chat-server" --limit=50

# Get service URLs
url-mcp:
	@gcloud run services describe ucp-mcp-server --region $(REGION) --format 'value(status.url)'

url-chat:
	@gcloud run services describe ucp-chat-server --region $(REGION) --format 'value(status.url)'

# Clean up
clean:
	docker-compose down -v
	docker rmi $(MCP_IMAGE):latest $(CHAT_IMAGE):latest 2>/dev/null || true

# Setup service account and secrets
setup: setup-service-account setup-secrets

setup-service-account:
	@echo "Setting up service account..."
	./scripts/setup-service-account.sh

setup-secrets:
	@echo "Creating secrets in Google Cloud Secret Manager..."
	@echo "Please enter your OpenAI API Key:"
	@read -s OPENAI_KEY && echo -n "$$OPENAI_KEY" | gcloud secrets create openai-api-key --data-file=- 2>/dev/null || echo "Secret already exists"
	@echo "Please enter your Shopify Access Token:"
	@read -s SHOPIFY_TOKEN && echo -n "$$SHOPIFY_TOKEN" | gcloud secrets create shopify-access-token --data-file=- 2>/dev/null || echo "Secret already exists"
	@echo "Please enter your Twilio Account SID:"
	@read -s TWILIO_SID && echo -n "$$TWILIO_SID" | gcloud secrets create twilio-account-sid --data-file=- 2>/dev/null || echo "Secret already exists"
	@echo "Please enter your Twilio Auth Token:"
	@read -s TWILIO_TOKEN && echo -n "$$TWILIO_TOKEN" | gcloud secrets create twilio-auth-token --data-file=- 2>/dev/null || echo "Secret already exists"
	@echo "Please enter your Twilio Phone Number (e.g., +1234567890):"
	@read -s TWILIO_PHONE && echo -n "$$TWILIO_PHONE" | gcloud secrets create twilio-phone-number --data-file=- 2>/dev/null || echo "Secret already exists"

# Health checks
health-mcp:
	@curl -s $(shell gcloud run services describe ucp-mcp-server --region $(REGION) --format 'value(status.url)')/healthz

health-chat:
	@curl -s $(shell gcloud run services describe ucp-chat-server --region $(REGION) --format 'value(status.url)')/healthz
