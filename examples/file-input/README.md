# File example: upload and save a PDF

The `save_report` capability accepts a PDF in the call, saves it into the `exchange` folder on the agent host under the name the caller sent, and returns a copy of the saved file. No database is needed.

## Run it

Put [agent.yaml](agent.yaml) and [gateway.env](gateway.env) in a directory next to the `onprest-agent` and `onprest-gateway` binaries, create the folder, and validate. Validation does not write anything into the folder.

```sh
mkdir -p exchange
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

## Upload a file

When `http://127.0.0.1:8080/healthz` reports `"agent_connected": true`, upload any PDF saved as `report.pdf`. The file goes in a binary part, and the `month` argument in a JSON part:

```sh
export ONPREST_API_KEY=orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM
curl --fail --show-error \
  -H "Authorization: Bearer $ONPREST_API_KEY" \
  --form 'document=@./report.pdf;type=application/pdf' \
  --form 'month="\"2026-10\"";type=application/json' \
  http://127.0.0.1:8080/api/v1/capabilities/save_report
```

The file is now `exchange/report.pdf`, and the response lists the saved copy with a `uri`. Download it and compare both with the original:

```sh
FILE_URI='paste result.files[0].uri here'
curl --fail --show-error -H "Authorization: Bearer $ONPREST_API_KEY" "$FILE_URI" --output returned-report.pdf
cmp report.pdf exchange/report.pdf
cmp report.pdf returned-report.pdf
```

Both comparisons print nothing when the bytes match. Uploading `report.pdf` again fails with `CONFLICT`, because the capability does not overwrite existing files.

Small files can also be sent inline in JSON or MCP as `{"name": ..., "mime_type": ..., "data": "<Base64>"}`, within the gateway's 1 MiB JSON limit. See [Files](https://docs.onprest.viewlegacy.com/capabilities/files#saving-uploaded-files) for save options and [API: Files](https://docs.onprest.viewlegacy.com/api/files#sending-files) for the upload forms.
