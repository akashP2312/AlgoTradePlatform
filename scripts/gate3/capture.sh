#!/bin/sh
# Print the current risk.book (KV bucket risk, key book) as JSON. Read-only: one
# KV get through the riskengine container's NATS client.
# Usage: scripts/gate3/capture.sh [container] > gate3/$(date +%F_%H%M).json
set -eu
docker exec -i "${1:-pmt-riskengine-1}" python - <<'EOF'
import asyncio, os, sys, nats
async def main():
    nc = await nats.connect(os.environ["NATS_URL"])
    e = await (await nc.jetstream().key_value("risk")).get("book")
    sys.stdout.write(e.value.decode())
    await nc.close()
asyncio.run(main())
EOF
