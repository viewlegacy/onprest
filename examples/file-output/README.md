# File example: download a report

The `download_report` capability returns a completed report from a folder on the agent host, as a download link. The caller never sees the folder or its path. No database is needed.

## Run it

Put [agent.yaml](agent.yaml) and [gateway.env](gateway.env) in a directory next to the `onprest-agent` and `onprest-gateway` binaries. Create the report the way a producer should, under a temporary name followed by a rename, then validate:

```sh
mkdir -p reports
printf 'completed report\n' > reports/report.tmp
mv reports/report.tmp reports/report.txt
./onprest-agent validate --config ./agent.yaml
```

The credentials are public; use them only on loopback. Start the gateway, then the agent in another shell:

```sh
set -a
. ./gateway.env
set +a
./onprest-gateway
```

```sh
./onprest-agent --config ./agent.yaml
```

## Call it

When `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{}' http://127.0.0.1:8080/api/v1/capabilities/download_report
```

The response lists one file, `report.txt`, with its size, SHA-256, and a `uri`. Download it with the same key:

```sh
REPORT_URI='paste result.files[0].uri here'
curl --fail --show-error -H "Authorization: Bearer $ONPREST_API_KEY" "$REPORT_URI" --output downloaded-report.txt
cat downloaded-report.txt
```

The file contains `completed report`. Downloading does not read the folder again: the link serves the copy taken during the call, until it expires or either binary restarts.

To get the file directly in the response instead of a link, set `delivery.rest: direct` in `agent.yaml` and restart the agent. See [Files](https://docs.onprest.viewlegacy.com/capabilities/files) for selecting files by pattern or date, and [API: Files](https://docs.onprest.viewlegacy.com/api/files#receiving-files) for delivery options.
