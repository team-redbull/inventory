#!/usr/bin/env bash
# Tear down the local dev environment.
set -euo pipefail

echo "Deleting kind cluster..."
kind delete cluster --name fleet-dev 2>/dev/null || true

echo "Stopping docker compose services..."
docker compose down -v 2>/dev/null || true

echo "Done."
