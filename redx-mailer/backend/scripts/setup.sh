#!/bin/bash

echo "🚀 Setting up RED-X Mailer Backend"

# Create necessary directories
mkdir -p uploads smtp tasks temp

# Check if .env exists
if [ ! -f .env ]; then
    echo "📝 Creating .env file from example..."
    cp .env.example .env
    echo "⚠️  Please update the .env file with your actual values!"
fi

# Check if service-account.json exists
if [ ! -f service-account.json ]; then
    echo "🔑 Please create service-account.json with your Google Service Account credentials"
    echo "You can create one at: https://console.cloud.google.com/iam-admin/serviceaccounts"
    echo ""
    echo "Required format:"
    cat << 'EOF'
{
  "type": "service_account",
  "project_id": "your-project-id",
  "private_key_id": "your-private-key-id",
  "private_key": "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
  "client_email": "your-service-account@your-project.iam.gserviceaccount.com",
  "client_id": "your-client-id",
  "auth_uri": "https://accounts.google.com/o/oauth2/auth",
  "token_uri": "https://oauth2.googleapis.com/token",
  "auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
  "client_x509_cert_url": "https://www.googleapis.com/robot/v1/metadata/x509/your-service-account%40your-project.iam.gserviceaccount.com"
}
EOF
    exit 1
fi

# Download dependencies
echo "📦 Downloading Go dependencies..."
go mod download

# Build the application
echo "🔨 Building application..."
go build -o redx-mailer .

echo "✅ Setup complete!"
echo ""
echo "To run locally:"
echo "  ./redx-mailer"
echo ""
echo "Environment variables in .env:"
echo "  SPREADSHEET_ID - Google Sheets ID"
echo "  SUPABASE_URL - Your Supabase URL"
echo "  SUPABASE_SERVICE_KEY - Your Supabase service key"