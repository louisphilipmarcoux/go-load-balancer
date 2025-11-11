#!/bin/sh
# This script runs before the load balancer to ensure Consul services are registered and healthy

set -e

echo '[STARTUP] Installing dependencies...'

echo '[STARTUP] Waiting for Consul leader...'
until curl -s http://consul:8500/v1/status/leader | grep -q '"'; do
  echo '[STARTUP] Still waiting for Consul...'
  sleep 1
done

echo '[STARTUP] Consul is up. Waiting for backends to be reachable...'

until curl -sf http://backend1:9001/ >/dev/null 2>&1; do
  echo '[STARTUP] Waiting for backend1:9001...'
  sleep 1
done

until curl -sf http://backend2:9002/ >/dev/null 2>&1; do
  echo '[STARTUP] Waiting for backend2:9002...'
  sleep 1
done

until curl -sf http://backend3:9003/ >/dev/null 2>&1; do
  echo '[STARTUP] Waiting for backend3:9003...'
  sleep 1
done

echo '[STARTUP] All backends are responding. Registering services...'

curl -X PUT http://consul:8500/v1/agent/service/register -d '{
  "ID": "api-service-1",
  "Name": "api-service",
  "Address": "backend1",
  "Port": 9001,
  "Check": {
    "TCP": "backend1:9001",
    "Interval": "10s",
    "Timeout": "2s"
  }
}' && echo '[STARTUP] ✓ Registered api-service'

curl -X PUT http://consul:8500/v1/agent/service/register -d '{
  "ID": "mobile-service-1",
  "Name": "mobile-service",
  "Address": "backend2",
  "Port": 9002,
  "Check": {
    "TCP": "backend2:9002",
    "Interval": "10s",
    "Timeout": "2s"
  }
}' && echo '[STARTUP] ✓ Registered mobile-service'

curl -X PUT http://consul:8500/v1/agent/service/register -d '{
  "ID": "web-service-1",
  "Name": "web-service",
  "Address": "backend3",
  "Port": 9003,
  "Check": {
    "TCP": "backend3:9003",
    "Interval": "10s",
    "Timeout": "2s"
  }
}' && echo '[STARTUP] ✓ Registered web-service'

echo '[STARTUP] Waiting for health checks to pass (5 seconds)...'
sleep 5

WAIT_ATTEMPTS=0
MAX_WAIT=30
while true; do
  WEB_COUNT=$(curl -s 'http://consul:8500/v1/health/service/web-service?passing=true' | jq 'length')
  API_COUNT=$(curl -s 'http://consul:8500/v1/health/service/api-service?passing=true' | jq 'length')
  
  echo "[STARTUP] web-service healthy: $WEB_COUNT, api-service healthy: $API_COUNT"
  
  if [ "$WEB_COUNT" -gt 0 ] && [ "$API_COUNT" -gt 0 ]; then
    echo '[STARTUP] ✓ Services are healthy!'
    break
  fi
  
  WAIT_ATTEMPTS=$((WAIT_ATTEMPTS+1))
  if [ $WAIT_ATTEMPTS -gt $MAX_WAIT ]; then
    echo '[STARTUP] ✗ ERROR: Timeout waiting for services'
    echo '[STARTUP] Consul services:'
    curl -s http://consul:8500/v1/catalog/services | jq .
    echo '[STARTUP] Web-service health:'
    curl -s http://consul:8500/v1/health/service/web-service | jq .
    echo '[STARTUP] API-service health:'
    curl -s http://consul:8500/v1/health/service/api-service | jq .
    exit 1
  fi
  
  sleep 1
done

echo '[STARTUP] Starting load balancer...'
exec /go-load-balancer "$@"