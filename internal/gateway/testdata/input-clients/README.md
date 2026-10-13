# Pinned real-client wire proof

This development-only harness generates an unmodified `typescript-fetch` client from the production OAS 3.1 schema and constraints, with only `servers` replaced by the test recorder address, compiles it, sends real HTTP, and drives the pinned Swagger UI through a real Chrome browser. A local test recorder captures original headers and bytes before forwarding to the production Gateway/Runner. It compares output names, order, bytes and SHA-256 separately from OAS document validation.

```sh
npm ci --ignore-scripts --prefix internal/gateway/testdata/input-clients
curl --fail --location \
  https://repo.maven.apache.org/maven2/org/openapitools/openapi-generator-cli/7.25.0/openapi-generator-cli-7.25.0.jar \
  --output /tmp/openapi-generator-7.25.0.jar
ONPREST_IT_INPUT_CLIENT_WIRE=1 \
ONPREST_IT_INPUT_CLIENT_GENERATOR_JAR=/tmp/openapi-generator-7.25.0.jar \
ONPREST_IT_INPUT_CLIENT_EVIDENCE_DIR=/tmp/onprest-input-client-evidence \
ONPREST_IT_INPUT_CLIENT_BROWSER='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' \
go test ./internal/agent -run '^TestInputDeliveryPinnedBrowserAndGeneratedClient$' -count=1 -v
```

The script verifies JAR SHA-256 `41ce4f6b07f196676439d710759fa1ced7a08066d06ff1bf314681470289efae`. Versions are Swagger UI 5.27.1, OpenAPI Generator 7.25.0, TypeScript 5.9.3 and Playwright Core 1.56.1. Generated options are `withoutRuntimeChecks=true`, `--type-mappings=null=null,Null=null`, and `--global-property=skipFormModel=false`; standard media-specific schema titles avoid request model name collisions. Generated sources are not patched. These settings retain server schema validation while mapping null to TypeScript null and retaining form models referenced by the generated API.

The canonical document gives the generated client a JSON request method; its multipart generation is not claimed as supported. Swagger UI uses real repeated binary parts. JavaScript only supports exact safe-range integers here, and this Swagger version sends JSON string parts as unquoted bytes: that rejected request is retained as an unsupported case. The verified UI workaround enters `"\"x\""` to produce the JSON string `"x"`; an empty optional field still emits an invalid empty JSON part. Neither limitation is converted to a passing compatibility claim. `wire.json`, the test `openapi.json` with only `servers` replaced by the recorder address, generated files/compiler log, and browser screenshots are written to the evidence directory. Dependencies, JRE, browser and the recorder are test tooling, not binary deployment requirements.
