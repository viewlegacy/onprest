# File download example

This example uses the two released binaries and a completed local text file, without a database. Its signing key and API credential are public demonstration values: run on loopback only. Generate your own signing key and API key for deployment as described in [Provisioning CLI](https://docs.onprest.viewlegacy.com/reference/cli).

Save [agent.yaml](agent.yaml) and [gateway.env](gateway.env) in a working directory and place the released `onprest-gateway` and `onprest-agent` binaries beside them. Run the following commands from that directory to prepare the source:

```sh
mkdir -p reports
printf 'completed report\n' > reports/report.tmp
mv reports/report.tmp reports/report.txt
./onprest-agent validate --config ./agent.yaml
```

Start Gateway with the example environment in one shell:

```sh
set -a
. ./gateway.env
set +a
./onprest-gateway
```

Start Agent in another shell:

```sh
./onprest-agent --config ./agent.yaml
```

The Agent user needs permission to create private `identity` and `spool` directories beside this configuration. Wait until the Gateway reports `"agent_connected": true`:

```sh
curl --fail --show-error http://127.0.0.1:8080/healthz
```

Call the capability using the example API key:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{}' http://127.0.0.1:8080/api/v1/capabilities/download_report
```

Copy the returned `result.files[0].uri` into `REPORT_URI`, then download the retained snapshot with the same Authorization header:

```sh
REPORT_URI='paste the returned URI here'
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  "$REPORT_URI" --output downloaded-report.txt
cat downloaded-report.txt
```

The file contains `completed report`. This GET does not repeat the folder read or execute a business capability. References expire and are invalidated on Agent reconnect or either binary's restart. For a direct REST body, change `delivery.rest` to `direct` and restart Agent; MCP delivery remains reference. See [File Output](https://docs.onprest.viewlegacy.com/api/files) for limits, inline MCP and error handling.
