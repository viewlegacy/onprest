# Install Onprest from a release archive

This archive contains the `onprest-gateway` and `onprest-agent` binaries for the
target named in `RELEASE-MANIFEST.txt`. It does not require a source checkout,
Go, Docker, or a native database client runtime.

Checksums and GitHub artifact attestations are available for optional download
verification; see <https://docs.onprest.viewlegacy.com/operations/deployment#binary-release-installation>.

The templates in this archive are intentionally incomplete. Add at least one
reviewed capability to `capabilities: {}` and replace every placeholder before
validating or starting the Agent. For a local trial, download the separate
`onprest-quickstart.tar.gz` asset from the same release and follow:
<https://docs.onprest.viewlegacy.com/quick-start>

After extracting the archive for production:

1. Copy `gateway.env.example` to a protected `gateway.env`, and copy
   `capability.yaml.example` to a protected `capability.yaml`.
2. Generate an Agent key pair and a capability-scoped API key:

   ```sh
   ./onprest-gateway create-agent-secret
   ./onprest-gateway create-key --name operator --capabilities YOUR_CAPABILITY_NAME
   ```

   On Windows, use the same commands with `.\onprest-gateway.exe`. Put the
   generated public key in `GATEWAY_AGENT_PUBLIC_KEY`, the private key in
   `gateway.agent_private_key`, and the generated `key_hash` and capability list
   in `GATEWAY_API_KEYS_JSON`. Deliver the plaintext `api_key` to its caller; do
   not put it in `gateway.env`.
3. Replace every example URL, database address, and credential in
   `capability.yaml`, then add at least one reviewed capability. DB capabilities
   use the configured database; CSV, HTTP, file and Wasm capabilities do not
   require a database. Update the API key capability allow-list to match the
   capabilities you added.
4. Provide private writable spool and persistent identity directories relative
   to the configuration, or set their runtime paths explicitly. Validate the
   completed configuration and referenced resources before starting the Agent:

   ```sh
   ./onprest-agent validate --config /absolute/path/capability.yaml
   ```

   On Windows, run `onprest-agent.exe validate --config
   C:\absolute\path\capability.yaml` instead.
5. The Gateway reads process environment variables; it does not read
   `gateway.env` itself. On Unix, load the file and start the Gateway:

   ```sh
   set -a
   . ./gateway.env
   set +a
   ./onprest-gateway
   ```

   On Windows PowerShell, set the two configured values in the process or
   service environment, then run `onprest-gateway.exe`:

   ```powershell
   $env:GATEWAY_AGENT_PUBLIC_KEY = '<generated-agent-public-key>'
   $env:GATEWAY_API_KEYS_JSON = '[{"name":"operator","key_hash":"<generated-key-hash>","capabilities":["YOUR_CAPABILITY_NAME"]}]'
   .\onprest-gateway.exe
   ```
6. Start the Agent in another session, or install it with the OS service manager:

   ```sh
   ./onprest-agent --config /absolute/path/capability.yaml
   # Or:
   ./onprest-agent service install --config /absolute/path/capability.yaml
   ./onprest-agent service start
   ```

   Use the `.exe` commands and a Windows path on Windows. Confirm the Gateway
   `/healthz` reports `agent_connected: true` before testing REST or MCP.

The Gateway and Agent must come from the same release. Release acceptance and
provenance are documented at
<https://docs.onprest.viewlegacy.com/operations/release-gate>.

For Wasm, build and test code on the author’s computer, then deploy the completed
module with its YAML. The Agent embeds the worker runtime; the installation
does not require Go or the SDK source. SDK versions use independent
`sdk/go/vX.Y.Z` tags; see <https://docs.onprest.viewlegacy.com/reference/cli#build-and-test-wasm-code>.

Keep cleanup records across restart, and use completed business files outside
the spool. Configure finite client/proxy timeouts for input, execution and
output. Recovery is documented at
<https://docs.onprest.viewlegacy.com/operations/troubleshooting>.
For Agent management limits and their defaults, see
<https://docs.onprest.viewlegacy.com/agent/capability-yaml#top-level-sections>.
