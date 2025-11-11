#!/bin/bash
#
# This script runs the end-to-end test suite against a live Docker environment.

# Exit immediately if any command fails
set -e

echo "--- 🐳 Starting Docker Compose Environment ---"
# Add the --build flag to force re-compiling the Go code
docker-compose up -d --build

# --- Cleanup ---
# This 'trap' ensures docker-compose down runs even if the tests fail
cleanup() {
    echo "--- 🧹 Cleaning Up Docker Environment ---"
    echo "--- 📋 Final LB Container Logs ---"
    docker-compose logs lb
    docker-compose down
}
trap cleanup EXIT

echo "--- ⏳ Waiting for services to become healthy ---"

# First, check if the startup script is even running
echo "Checking for startup script execution..."
sleep 3
if docker-compose logs lb | grep -q "\[STARTUP\]"; then
    echo "✓ Startup script is running"
else
    echo "✗ WARNING: Startup script not detected! Showing all LB logs:"
    docker-compose logs lb
fi

# Robust wait loop
echo "Waiting for L7 service discovery to complete..."
ATTEMPTS=0
MAX_ATTEMPTS=15

# Use curl: -s (silent), -k (insecure), -o /dev/null (discard body), -w (write-out http_code)
until [ "$(curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:8443)" == "200" ]; do
    ATTEMPTS=$((ATTEMPTS+1))
    if [ $ATTEMPTS -gt $MAX_ATTEMPTS ]; then
        echo "E2E: Timed out waiting for load balancer to become healthy."
        exit 1
    fi
    echo "Waiting for LB... (attempt $ATTEMPTS/$MAX_ATTEMPTS)"
    sleep 2
done

echo "✅ Load balancer is healthy and responding 200."


echo "--- 🚀 Running E2E Tests ---"

# Source the .env file to load ADMIN_TOKEN for the Go test
if [ -f .env ]; then
    echo "Sourcing .env file for Go tests..."
    # This command exports all variables from .env into the script's environment
    set -a
    source .env
    set +a
else
    echo "Warning: .env file not found. Admin API tests might fail."
fi

# Use the relative file path to the e2e test package
go test -v -tags=e2e ./test/e2e

echo "--- ✅ E2E Tests Passed ---"

# The 'trap' will handle cleanup